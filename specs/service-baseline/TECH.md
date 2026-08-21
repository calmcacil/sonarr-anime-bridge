# Tech Spec: Sonarr Anime Bridge Service Baseline

## Context

This document records the implementation that realizes the compatibility contract in [PRODUCT.md](./PRODUCT.md). It describes the shipped baseline rather than a proposed redesign. Code references are pinned to commit [`516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168`](https://github.com/calmcacil/sonarr-anime-bridge/tree/516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168).

- [`cmd/server/main.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168/cmd/server/main.go) owns process lifecycle, HTTP routing, request validation, degradation responses, component health evaluation, and the image healthcheck client.
- [`internal/config/config.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168/internal/config/config.go) owns environment parsing and safety validation, including the configured prewarm-year list supplied to health evaluation.
- [`internal/cache/cache.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168/internal/cache/cache.go) owns the SQLite year cache, cache metrics, and the read-only multi-year readiness query used by health.
- [`internal/scheduler/scheduler.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168/internal/scheduler/scheduler.go) owns fetch coordination, processing, refresh, pruning, mapping lifecycle, and resolver availability.
- [`internal/anilist/anilist.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168/internal/anilist/anilist.go), [`internal/filter/filter.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168/internal/filter/filter.go), and [`internal/mapping`](https://github.com/calmcacil/sonarr-anime-bridge/tree/516bf1ae3fed8b8c6f0d05e80451e4a6e6f49168/internal/mapping) implement upstream ingestion, filtering, and TVDB resolution.

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

`data` is the raw JSON serialization of the full AniList year query. There are no season, category, format, or resolved-output cache keys. This is the architectural invariant enabling runtime filtering and mapping refresh without AniList refetches.

`GetYearContext` records atomic hit/miss metrics and updates `last_hit` with a five-minute write debounce. Freshness is 24 hours for the current year and seven days otherwise. SQLite busy operations receive bounded retries; a startup database stuck busy after a previous crash may be removed with its sidecars and recreated because its contents are recoverable upstream.

`HasYearsContext` performs one read-only query for the configured prewarm years. It deduplicates repeated years, treats an empty input as ready, and leaves hit/miss counters and `last_hit` untouched; health uses its error to distinguish unreachable SQLite from a reachable cache that is still warming.

The ten-minute maintenance pass queries stale years using one-day/current and seven-day/past thresholds, refreshes each with a two-minute context, prunes rows older than 14 days by last access, and vacuums at most daily.

### AniList ingestion

A single `anilist.Client` fetches `ANIME` records for a `seasonYear`, sorted by popularity, independent of requested season and format. It requests 50 records per page with a maximum of 100 pages and bounds response/error body sizes.

The process-wide limiter permits one request every 700 ms. HTTP requests use a 30-second client timeout. Retry handling permits five attempts, honors `Retry-After` up to two minutes, applies exponential backoff with plus or minus 25 percent jitter, and enforces a five-second post-429 gap for 30 seconds.

`Scheduler.FetchAndStore` coordinates callers with a per-year `sync.Map` in-flight record. The first caller fetches and stores; waiters receive the same completion error. Fetch work has a two-minute ceiling and is canceled on application shutdown.

### Processing pipeline

`Scheduler.ProcessContext` executes this sequence:

1. Decode cached raw JSON.
2. For `WINTER`, read prior-year cache, season-filter it, retain December starts, and merge by unique AniList ID.
3. Filter the merged set by requested season (`ALL` bypasses this step).
4. Keep configured AniList formats.
5. Remove durations of ten minutes or less and configured tags.
6. Apply the three-month future filter when enabled.
7. Apply first-season filtering for `series-new`.
8. Resolve TVDB IDs and construct output records.

Season fallback for records without AniList season metadata is month-based: winter is December-March, spring April-June, summer July-September, and fall October-November. Records with explicit season metadata use that value instead.

### Mapping lifecycle

`mapping.LoadOrFetch` manages a zstd-compressed anibridge JSON file and adjacent metadata. Downloads are limited to 50 MiB compressed and 250 MiB decoded. URL and redirect hosts pass the shared allowlist. Conditional `HEAD`/`GET` checks use ETag and related metadata, with a 60-second HTTP timeout and local-cache fallback on upstream errors.

The parser builds MAL-to-TVDB and AniList-to-TVDB maps. For ambiguous mapping entries it prefers season-one scope and otherwise uses the candidate with the highest episode count. `Resolver` stores the active mapping in `atomic.Pointer`; refresh atomically swaps a complete immutable map. Lookups prefer MAL and then AniList.

The scheduler checks mappings every 24 hours. While unloaded, it retries loading every minute. A failed refresh leaves an existing resolver active; the absence of any active resolver drives PRODUCT invariants 16 and 19.

### Configuration and runtime safety

`config.LoadQuiet` applies PRODUCT invariants 29-31. Path validation allows `:memory:` only for `CACHE_DB_PATH`; persistent cache and mapping paths must be absolute paths under `/data` or the system temporary directory. Runtime directory validation checks both parent directories before opening either persistent resource, preventing partial startup side effects.

The Docker build compiles a static Go binary for `TARGETOS`/`TARGETARCH`, then copies it and an owned `/data` directory into `gcr.io/distroless/static-debian13:nonroot`. Compose sets the runtime identity with `user:` and mounts appdata at `/data`.

Release Please owns version and changelog updates. The trusted release workflow publishes the created tag for Linux `amd64` and `arm64`, including exact, minor, major, and latest tag families. PR workflows do not receive release credentials.

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
- PRODUCT 35-38: `make check` validates supported builds and local CI gates; CI validates Docker/build workflow structure. `docs/PREFLIGHT_TEST.md` defines native regression, container lifecycle, and live integration checks for behavioral changes.
- Every PR must run `make check`. Filtering, season, resolution, sorting, or pipeline changes additionally run `./testdata/native-regression.sh`; container/lifecycle changes use the documented Docker regression; upstream end-to-end coverage is opt-in with `INTEGRATION=1 go test -run TestIntegration ./... -v`.

## Risks and mitigations

- Upstream data changes can alter list membership without a code change. Native regression compares TVDB ID sets against the latest release and requires investigation of material differences.
- Empty-on-miss-failure is intentionally indistinguishable from a legitimately empty year to Sonarr. Health and structured logs remain the operator diagnostics; changing the list response requires a new product decision.
- The first winter request can omit prior-December entries while background backfill runs. Tests must preserve this non-blocking behavior and verify later convergence.
- Raw-year cache compatibility is load-bearing. Introducing filtered or resolved cache entries would make configuration/mapping updates stale and requires an explicit replacement design and migration plan.
- Rootless bind mounts cannot be repaired inside the distroless image. Startup validation must remain ahead of SQLite and mapping side effects.

## Parallelization

No implementation work is proposed by this baseline. Future changes should split only along established ownership boundaries (`config`/HTTP, cache/scheduler, AniList/mapping, container/release) and keep contract-changing integration and final validation centralized.
