# Tech Spec: Dockerized Sonarr Seasonal Lists Service

## Architecture

```text
cmd/server/main.go
  ├── internal/config/        → env-var configuration (no YAML/CLI)
  ├── internal/cache/         → SQLite year-cache (modernc.org/sqlite)
  ├── internal/scheduler/     → pipeline + background refresh goroutines
  ├── internal/anilist/       → AniList GraphQL client (paginated, rate-limited)
  ├── internal/filter/        → on-the-fly filtering (season, format, duration, tags, future)
  └── internal/mapping/       → TVDB ID resolver (anibridge, atomic.Pointer)
```

**Key design**: The server fetches full-year data from AniList (all seasons, all
formats) and caches the raw JSON per year in `year_cache(year)`. Filtering and
TVDB resolution happen on-the-fly per request. No per-season or per-category
cache entries.

## Core Pipeline

```text
request → db.GetYear(year)
  ├─ MISS → synchronous FetchAndStore(year)
  |        └─ still no data after fetch → return []
  ├─ WINTER + prior year missing → trigger async FetchAndStore(year-1)
  └─ HIT → sched.ProcessContext(rawData, season, year, category)
       ├─ Unmarshal raw JSON
       ├─ Winter overflow merge (December starts from prior year, only when season=WINTER)
       ├─ FilterBySeason → select matching season (`ALL` bypasses)
       ├─ FilterByFormats → keep configured formats
       ├─ Filter → exclude by duration ≤10 min and tags
       ├─ FilterFuture(3) → exclude shows >3 months out
       ├─ FilterFirstSeason → exclude prequels/parents (series-new only)
       ├─ ResolveBatch → AniList IDs → TVDB IDs
       └─ Marshal → JSON response
```

## Component Details

### `internal/config/`

Validation behavior on load:

- `PORT` is validated and clamped to `1..65535`; invalid values fall back to `8080`
- `CACHE_DB_PATH`, `MAPPING_PATH` require plain absolute paths under `/data` or system temp
- `MAPPING_URL` must be HTTPS; non-default hosts are warned; insecure and unsafe URLs are rejected
- `PREWARM_YEARS` skips invalid/out-of-range entries; if all entries are skipped, defaults to current year

Startup also verifies the parent directories for `CACHE_DB_PATH` and
`MAPPING_PATH` exist, are directories, and are readable and writable by the
runtime user. Failure stops the process before opening SQLite or downloading
mappings.

| Field | Env Var | Default |
|-------|---------|---------|
| `Port` | `PORT` | `8080` |
| `PrewarmYears` | `PREWARM_YEARS` | `[current year]` |
| `IncludeTypes` | `INCLUDE_TYPES` | `['TV','ONA']` |
| `ExcludeTags` | `EXCLUDE_TAGS` | `nil` |
| `FilterFutureEnabled` | `FILTER_FUTURE_ENABLED` | `true` |
| `CacheDBPath` | `CACHE_DB_PATH` | `/data/cache.db` |
| `LogLevel` | `LOG_LEVEL` | `"info"` |
| `AnibridgeMappingPath` | `MAPPING_PATH` | `/data/anibridge_mappings.json.zst` |
| `AnibridgeURL` | `MAPPING_URL` | anibridge release URL |
| `DebugEndpointsEnabled` | `DEBUG_ENDPOINTS_ENABLED` | `false` |
| `AdminToken` | `ADMIN_TOKEN` | `""` |

### `internal/cache/`

Pure-Go SQLite (WAL mode). One row per year:

```sql
CREATE TABLE year_cache (year INTEGER PRIMARY KEY, data BLOB, fetched_at INTEGER, last_hit INTEGER DEFAULT 0);
```

Freshness: 24 h for current year, 7 days for past years. Hit/miss counts via
`atomic.Int64`. Key methods: `GetYear`, `SetYear`, `HasYear`, `Clear`,
`NeedsRefreshYears`, `PruneStaleYears`, `Stats`, `Ping`.

### `internal/scheduler/`

Owns the fetch → cache → filter → resolve pipeline. Background goroutines:

- **Stale refresh** (every 10 min): refreshes years stale beyond 1 day (current) or 7 days (past), prunes entries with last_hit >14 days old, vacuums SQLite.
- **Mapping refresh** (every 24 h): HEAD-checks upstream anibridge ETag, downloads if changed, swaps atomically.

In-flight deduplication: `sync.Map` prevents concurrent fetches for the same year.
Panic recovery and context-cancellation-aware in all goroutines.

### `internal/anilist/`

Paginated GraphQL client. 50 results/page. Rate limiting: token bucket with
700ms minimum interval. `FetchYear` returns all anime for a year regardless of
season/format.

#### Retry behavior

- Exponential backoff: `2s`, `4s`, `8s`, `16s`, `32s` (+/-25% jitter), up to 5 attempts.
- `Retry-After` headers are honored and clamped to service maximum when present.
- After HTTP `429`, a 5-second post-limit gap is enforced for 30 seconds.

Show predicates (used by filters): `IsSeries`, `IsNew`, `SkipByDuration`,
`HasTag`, `IsWithinMonths`, `IsWinterStart`, `DisplayTitle`.

### `internal/filter/`

All filtering is on-the-fly from cached raw data. Functions:
`FilterBySeason` (with month-based fallback for empty season field),
`FilterByFormats`, `Filter` (duration + tags), `FilterFuture`, `FilterFirstSeason`.

### `internal/mapping/`

Zstd-compressed JSON mapping (~8 MB compressed). `LoadOrFetch` uses conditional
HTTP (`HEAD`/`GET`) with allowlisted hosts (`github.com`, GitHub object hosts),
strict URL/path validation, redirect host checks, and fallback to cache on error.
TVDB extraction prefers `s1` scope, falls back to highest episode count.

Resolver uses `atomic.Pointer[AnibridgeMapping]` — mapping swaps don't block
in-flight lookups. Resolution order: MAL first, AniList fallback.

### `cmd/server/main.go`

Stdlib `net/http`. Startup: load config → open cache → load resolver → start
background goroutines → start HTTP server (listens immediately) → prewarm
configured years (async, already bound to listeners).

Graceful shutdown: context cancel → wait prewarm goroutine → `server.Shutdown(10s)`
→ `sched.Wait(5s)`.

Endpoints: `/list`, `/health`, `/cache/stats`, `/cache/clear`. Middleware:
logging (method, path, status, duration), panic recovery, and method checks.

## Docker

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS builder
COPY . . && RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /server ./cmd/server
RUN mkdir -p /out/data && chmod 0775 /out/data

FROM --platform=$TARGETPLATFORM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /server /server
COPY --from=builder --chown=65532:65532 /out/data /data

EXPOSE 8080

VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/server", "--healthcheck"]

ENTRYPOINT ["/server"]
```

CI builds `linux/amd64` and `linux/arm64`.

The final image is rootless and distroless. It has no shell, package manager,
`su-exec`, runtime user creation, or startup ownership repair. `/data` is the
only expected persistent writable path. Docker/Compose users set the runtime
UID/GID with `user: "${PUID:-1000}:${PGID:-1000}"` and must ensure bind-mounted
appdata is readable and writable by that UID/GID before startup.

Published images use immutable digests. Release automation publishes:

- `latest` for the newest stable release.
- Major tracks such as `v2`.
- Minor tracks such as `v2.12`.
- Exact release tags such as `v2.12.0`.

The image does not self-update at startup. Updates and rollbacks happen outside
the container with `docker compose pull && docker compose up -d`, Watchtower,
Renovate, Dependabot, or GitOps/deployment automation.

### CLI

- `--healthcheck`: used by container healthcheck to validate `/health` response.

## Dependencies

| Direct | Purpose |
|--------|---------|
| `modernc.org/sqlite` | Pure-Go SQLite |
| `github.com/klauspost/compress/zstd` | Zstd decompression |

(Plus indirect transitive deps from sqlite.)

## File Layout

| Path | Purpose |
|------|---------|
| `cmd/server/main.go` | HTTP server entrypoint |
| `internal/config/config.go` | Env-var config |
| `internal/cache/cache.go` | SQLite year cache |
| `internal/scheduler/scheduler.go` | Pipeline + background workers |
| `internal/anilist/anilist.go` | AniList GraphQL client |
| `internal/filter/filter.go` | On-the-fly filtering |
| `internal/mapping/anibridge.go` | Mapping loader/parser |
| `internal/mapping/resolve.go` | TVDB resolver |
| `internal/testutil/testutil.go` | Shared test helpers |
| `Dockerfile` | Multi-stage multi-arch build |
| `docker-compose.yml` | Quick-start composition |
