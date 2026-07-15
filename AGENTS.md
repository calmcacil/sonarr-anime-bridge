# sonarr-anime-bridge AGENTS

## What this repo is

- Long-running Go HTTP service (`cmd/server/main.go`) that serves `/list` for Sonarr.

## High-signal architecture

- **Core packages**: `internal/config`, `internal/cache`, `internal/scheduler`, `internal/anilist`, `internal/filter`, `internal/mapping`.
- **Data path**: AniList API -> `Scheduler.FetchAndStore` -> `year_cache` row (SQLite) -> on-demand filtering -> TVDB resolution -> JSON response.
- **No per-season DB rows**: cache is one row per year (`internal/cache/cache.go:107`).
- **Resolver** uses atomic swaps in `internal/mapping/resolve.go` so mapping refresh is lock-free for readers.
- **Entrypoint behavior**: config load -> open cache -> load resolver -> start HTTP server -> prewarm configured years (prewarm runs after listen start).

## Runtime behavior that is easy to miss

- `/list` does **synchronous** fetch on cache miss; if fetch fails, it still returns `[]`.
- If cached data is stale, `/list` returns stale rows and schedules async refresh (`stale_refresh`).
- `WINTER` requests always attempt prior-year backfill (`year-1`) and merge prior-year `DEC` starts.
- If resolver is not loaded, `/list` returns 503 and `/health` returns 503 (`degraded`).
- `/cache/stats` and `/cache/clear` are debug endpoints gated by `DEBUG_ENDPOINTS_ENABLED`; `ADMIN_TOKEN` enables bearer auth.
- Docker image starts `/server` directly from a rootless distroless runtime; `/data` must be readable and writable by the runtime UID/GID.
- Startup hard-stops before opening SQLite or downloading mappings if cache/mapping parent directories are missing, not directories, or not readable/writable.
- `POST /cache/clear` exists, `GET /cache/clear` is not supported.

## Configuration (env)

- `PORT` (default `8080`), `CACHE_DB_PATH` (default `/data/cache.db`), `LOG_LEVEL`.
- `PREWARM_YEARS` is CSV years; default current year; invalid list falls back to current year.
- `INCLUDE_TYPES`, `EXCLUDE_TAGS`, `FILTER_FUTURE_ENABLED` (default true, 3-month window).
- `MAPPING_PATH` default `/data/anibridge_mappings.json.zst`, `MAPPING_URL` default anibridge GitHub release URL.
- `PUID` and `PGID` are Docker/Compose `user:` variables only; they are not application environment variables.

## Useful source-of-truth files

- `internal/config/config.go` for env parsing/validation.
- `internal/scheduler/scheduler.go` for pipeline + background cadence (stale refresh every 10m, mapping refresh every 24h).
- `internal/anilist/anilist.go` for rate limit/backoff behavior.
- `internal/mapping/anibridge.go` for mapping download/cache/ETag logic.
- `docs/PREFLIGHT_TEST.md` for verification commands.

## Commands

- **Must-run locally for PRs**: `make check`.
- `go build ./...` and `go test -race ./...` are included in the local and CI gates.
- `golangci-lint run ./...` with config from `.golangci.yml`.
- Docker build: `DOCKER_BUILDKIT=1 docker build --platform=linux/arm64 --build-arg TARGETOS=linux --build-arg TARGETARCH=arm64 -t sonarr-anime-bridge:test-arm64 .`
- Native regression: `./testdata/native-regression.sh`.
- Integration tests: `INTEGRATION=1 go test -run TestIntegration ./... -v`.

## Release/workflow expectations

- Release Please owns routine versions and `CHANGELOG.md`; do not edit them manually.
- The trusted `Release` workflow publishes the exact created tag to GHCR for Linux amd64/arm64.
- Pull requests use Conventional Commit titles; the stable required check is `Required`.
- Release credentials are `RELEASE_APP_CLIENT_ID` and `RELEASE_APP_PRIVATE_KEY`; they never belong in PR workflows.
- See `docs/CI_RELEASES.md` for first-release verification, failed-publication recovery, and rollback.
