# Tech Spec: Sonarr Anime Bridge Service Baseline

## Context

This document records the implementation that realizes the compatibility contract in [PRODUCT.md](./PRODUCT.md). Code references resolve to the same revision as this specification.

- [`cmd/server/main.go`](../../cmd/server/main.go) owns process lifecycle, HTTP routing, request validation, degradation responses, component health evaluation, and the image healthcheck client.
- [`internal/config/config.go`](../../internal/config/config.go) owns environment parsing and safety validation, including the configured prewarm-year list supplied to health evaluation.
- [`internal/cache/cache.go`](../../internal/cache/cache.go) owns the SQLite year cache, cache metrics, and the read-only multi-year readiness query used by health.
- [`internal/scheduler/scheduler.go`](../../internal/scheduler/scheduler.go) owns fetch coordination, processing, refresh, pruning, mapping lifecycle, and resolver availability.
- [`internal/anilist/anilist.go`](../../internal/anilist/anilist.go), [`internal/filter/filter.go`](../../internal/filter/filter.go), and [`internal/mapping`](../../internal/mapping) implement upstream ingestion, filtering, and TVDB resolution.

## Implemented design

### Process lifecycle and HTTP boundary

`main.run` loads and logs configuration, validates data directories, opens SQLite, constructs the scheduler, attempts the initial resolver load, registers handlers, starts background workers, starts `http.Server`, and finally launches prewarm in a goroutine. The server uses 10-second read, 120-second write, and 30-second idle timeouts. Logging and panic-recovery middleware wrap all routes.

The handlers implement PRODUCT invariants 1-23 directly. Request parsing occurs before cache access. JSON success responses and structured health responses set `application/json`; `http.Error` paths use Go's standard text response behavior. Health evaluation receives a copied `cfg.PrewarmYears` slice so configuration state cannot be mutated by a request.

### Health evaluation

The private health response model contains top-level `status`, optional existing `reason`, and `checks.cache`/`checks.resolver` objects. Cache status is one of `ok`, `warming`, or `unhealthy`; resolver status is `ok` or `degraded`. The cache check calls `Cache.HasYearsContext` once with the configured prewarm years; the query is read-only, deduplicates repeated years, treats an empty list as ready, and does not affect hit/miss counters or `last_hit`. A query error reports cache `unhealthy` and aggregate HTTP `503`.

Resolver availability is evaluated independently through `Scheduler.ResolverLoaded`. A loaded resolver reports `ok`; an unloaded resolver reports `degraded` and makes aggregate health HTTP `503` with the existing `reason`. When SQLite is reachable and the resolver is loaded, aggregate health is `ok`/HTTP `200` whether cache readiness is `ok` or informational `warming`. Health payloads contain no query errors, paths, URLs, configured years, identifiers, or other request-specific data. The existing healthcheck continues to require only HTTP `200`, and the server stack suppresses the body for `HEAD`.

A cache miss receives a 90-second request-derived fetch context. On success the handler rereads SQLite before processing. Stale refresh and missing-prior-year winter backfill use scheduler-owned background tasks with 90-second limits so they do not block the originating request but remain tied to application shutdown.

### Data model and cache

SQLite runs in WAL mode through `modernc.org/sqlite`. The primary cache schema is one replaceable row per year:

```sql
CREATE TABLE year_cache (
  year INTEGER PRIMARY KEY,
  data BLOB,
  fetched_at INTEGER,
  last_hit INTEGER DEFAULT 0
);
```

`data` is the JSON serialization of the unfiltered AniList year query, containing only fields needed for filtering, titles, and resolution. Older payloads with extra fields remain readable. There are no season, category, format, or resolved-output cache keys. This is the architectural invariant enabling runtime filtering and mapping refresh without AniList refetches.

`GetYearContext` records atomic hit/miss metrics and updates `last_hit` asynchronously with a five-minute write debounce. Freshness is 24 hours for the current year and seven days otherwise. SQLite busy operations receive bounded retries; startup failures do not remove the database or its sidecars.

`HasYearsContext` performs one read-only query for the configured prewarm years. It deduplicates repeated years, treats an empty input as ready, and leaves hit/miss counters and `last_hit` untouched; health uses its error to distinguish unreachable SQLite from a reachable cache that is still warming.

The ten-minute maintenance pass queries stale years using one-day/current and seven-day/past thresholds, refreshes each with a two-minute context, prunes rows older than 14 days by last access, and vacuums at most daily.

### AniList ingestion

A single `anilist.Client` fetches `ANIME` records for a `seasonYear`, sorted by popularity, independent of requested season and format. It requests 50 records per page with a maximum of 100 pages and bounds response/error body sizes.

Page and pagination fields must be present, and media must be an array. Missing response structures cannot replace stale cached data with a fresh empty year. Unused episodes, genres, status, and relation-node data are not requested.

The process-wide limiter permits one request every 700 ms. HTTP requests use a 30-second client timeout. Retry handling permits five attempts, honors `Retry-After` up to two minutes, applies exponential backoff with plus or minus 25 percent jitter, and enforces a five-second post-429 gap for 30 seconds.

`Scheduler.FetchAndStore` coordinates callers with a per-year `sync.Map` in-flight record. The first caller fetches and stores; waiters receive the same completion error. Fetch work has a two-minute ceiling and is canceled on application shutdown.

### Processing pipeline

`Scheduler.Process` executes this sequence:

1. Receive request-owned shows decoded once by the HTTP boundary, with separately decoded prior-year data when requested.
2. For `WINTER`, season-filter prior-year shows, retain December starts, and merge by unique AniList ID.
3. Filter the merged set by requested season (`ALL` bypasses this step).
4. Keep configured AniList formats.
5. Remove durations of ten minutes or less and configured tags.
6. Apply the three-month future filter when enabled.
7. Apply first-season filtering for `series-new`.
8. Resolve TVDB IDs and construct output records.

Season fallback for records without AniList season metadata is month-based: winter is December-March, spring April-June, summer July-September, and fall October-November. Records with explicit season metadata use that value instead.

Filters compact request-owned slices in place and preserve order. Shared fixtures or retained slices must be cloned before filtering. Tag matching uses `strings.EqualFold`. Optional mapping-discovery tracking runs in one scheduler-owned background task with a five-second deadline, never on the response path. Tracking is best-effort: requests while tracking is busy leave discovery to subsequent requests. Batch inserts prepare their statement once per transaction. Scheduler waiting begins only after new background tasks are closed.

### Mapping lifecycle

`mapping.LoadOrFetch` manages a zstd-compressed anibridge JSON file and adjacent metadata. Downloads are limited to 50 MiB compressed and 250 MiB decoded. URL and redirect hosts pass the shared allowlist. Conditional `HEAD`/`GET` checks use ETag and related metadata, with a 60-second HTTP timeout and local-cache fallback on upstream errors.

Downloads are parsed before atomically replacing the cached file. The parser builds MAL-to-TVDB and AniList-to-TVDB maps. For ambiguous mapping entries it prefers season-one scope, then the highest episode count, then the lowest TVDB ID. `Resolver` stores the active mapping in `atomic.Pointer`; refresh atomically swaps a complete immutable map. Lookups prefer MAL and then AniList.

The scheduler checks mappings every 24 hours. While unloaded, it retries loading every minute. A failed refresh leaves an existing resolver active; the absence of any active resolver drives PRODUCT invariants 16 and 19.

### Configuration and runtime safety

`config.LoadQuiet` applies PRODUCT invariants 29-31. Path validation allows `:memory:` only for `CACHE_DB_PATH`; persistent cache and mapping paths must be absolute paths under `/data` or the system temporary directory. Runtime directory validation checks both parent directories before opening either persistent resource, preventing partial startup side effects.

The Docker build compiles a static Go binary for `TARGETOS`/`TARGETARCH`, then copies it and an owned `/data` directory into `gcr.io/distroless/static-debian13:nonroot`. Compose sets the runtime identity with `user:` and mounts appdata at `/data`.

Release Please owns version and changelog updates. The trusted release workflow publishes the created tag for Linux `amd64` and `arm64`, including exact, minor, major, and latest tag families. PR workflows do not receive release credentials.

`scripts/release_scope.py` implements PRODUCT invariant 39 before the coordinator obtains an App token. It compares production Go source, modules, Docker build configuration, licenses/notices, and the historical entrypoint by Git blob and mode against the highest published stable tag. Automation, documentation, tests, and version metadata are excluded; net-unchanged inputs suppress release coordination. Inventory, history, or build-layout errors fail closed. New embedded assets or image inputs require extending this explicit policy.

`scripts/ci_scope.py` keeps full validation on ordinary PRs, merge groups, and manual runs, but validates trusted Release Please metadata-only PRs without repeating application tests or container builds. CodeQL is mandatory in either scope and separately updates its default-branch baseline on main pushes. The stable `Required` aggregate accepts only the exact expected success/skip results. Native container builds replace duplicate standalone CI cross-builds; disposable registry promotion runs only for build/release automation changes or manual CI. Published-image verification and weekly security scans retain all gates. See [`docs/CI_RELEASES.md`](../../docs/CI_RELEASES.md) for the event policy and [`docs/RELEASE_AUDIT.md`](../../docs/RELEASE_AUDIT.md) for the preserved historical inventory.

## End-to-end flow

```text
Sonarr GET /list
  -> validate method and query
  -> require loaded resolver
  -> SQLite GetYear
       miss: deduplicated synchronous AniList fetch -> SetYear -> reread
       stale: retain data and schedule refresh
  -> optional prior-year winter merge/background fetch
  -> season -> format -> duration/tag -> future -> category filters
  -> atomic mapping lookup (MAL, then AniList)
  -> JSON [{tvdbId,title}]
```

Health `GET /health`
  -> one read-only `HasYearsContext` query for configured prewarm years
  -> independent `ResolverLoaded` check
  -> nested `status` plus `checks.cache.status` and `checks.resolver.status`
  -> aggregate HTTP `200`/`ok`, `503`/`degraded`, or `503`/`unhealthy`

## Testing and validation

- PRODUCT 1-23: `cmd/server/main_test.go` covers methods, validation, cache miss, degradation, structured component health, debug authorization, and error responses.
  Health cases cover ready and warming caches, unloaded resolvers, cache query failure, safe JSON fields, and `HEAD` body suppression.
- `internal/cache/cache_test.go` covers `HasYearsContext` readiness, duplicate and empty prewarm inputs, cancellation/errors, and the absence of hit/miss or `last_hit` mutation.
- PRODUCT 6-10 and 13-15: `internal/filter/filter_test.go` and `internal/scheduler/scheduler_test.go` cover filter boundaries, winter overflow, category behavior, and in-flight coordination.
- PRODUCT 24-28: `internal/cache/cache_test.go`, `internal/anilist/anilist_test.go`, and `internal/mapping/mapping_test.go` cover freshness, pruning, retry/rate-limit behavior, mapping parsing/fallback, and atomic resolution.
- PRODUCT 29-34: `internal/config/config_test.go` and `cmd/server/main_test.go` cover defaults, invalid input fallback, path/URL validation, startup directory checks, and lifecycle behavior.
- PRODUCT 35-39: `make check` validates supported builds and local CI gates; CI validates Docker/build workflow structure. `scripts/test_ci_scope.py` covers scope trust, metadata validity, actual Git diffs, and aggregate result combinations; `scripts/test_release_scope.py` covers release input equality, add/delete/revert changes, stable-version selection, and authority guards. `docs/PREFLIGHT_TEST.md` defines deterministic pipeline, native regression, and container lifecycle checks for behavioral changes.
- Every PR must run `make check`. The fixture-driven HTTP matrix covers exact ordered results for all seasons and both categories; focused AniList tests cover pagination, malformed responses, retry, and cancellation. Filtering, season, resolution, sorting, or pipeline changes additionally run `./testdata/native-regression.sh`. This optional live comparison checks full ordered responses against the latest release with no automatic drift tolerance. Container/lifecycle changes use the documented Docker regression.
- `go test -run '^$' -bench BenchmarkListHit -benchmem -count=5 ./cmd/server` measures the 600-show warm HTTP path with the shared pipeline fixture. Benchmarks report time and allocation; CI does not enforce machine-dependent timing thresholds.

## Risks and mitigations

- Upstream data changes can alter list membership and ordering without a code change. Native regression requires review of any difference; the deterministic fixture matrix remains the correctness gate.
- Empty-on-miss-failure is intentionally indistinguishable from a legitimately empty year to Sonarr. Health and structured logs remain the operator diagnostics; changing the list response requires a new product decision.
- The first winter request can omit prior-December entries while background backfill runs. Tests must preserve this non-blocking behavior and verify later convergence.
- Raw-year cache compatibility is load-bearing. Introducing filtered or resolved cache entries would make configuration/mapping updates stale and requires an explicit replacement design and migration plan.
- Rootless bind mounts cannot be repaired inside the distroless image. Startup validation must remain ahead of SQLite and mapping side effects.

## Parallelization

No implementation work is proposed by this baseline. Future changes should split only along established ownership boundaries (`config`/HTTP, cache/scheduler, AniList/mapping, container/release) and keep contract-changing integration and final validation centralized.
