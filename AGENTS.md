# sonarr-anime-bridge AGENTS

## What this repo is

- Long-running Go HTTP service (`cmd/server/main.go`) that serves `/list` for Sonarr.

## High-signal architecture

- **Core packages**: `internal/config`, `internal/cache`, `internal/scheduler`, `internal/anilist`, `internal/filter`, `internal/mapping`.
- **Data path**: AniList API -> `Scheduler.FetchAndStore` -> `year_cache` row (SQLite) -> on-demand filtering -> TVDB resolution -> JSON response.
- **No per-season DB rows**: cache is one row per year (`internal/cache/cache.go:107`).
- **Resolver** uses atomic swaps in `internal/mapping/resolve.go` so mapping refresh is lock-free for readers.
- **Entrypoint behavior**: register signal cancellation -> config load -> open cache -> cancellable resolver load -> start HTTP server -> prewarm configured years (prewarm runs after listen start).

## Runtime behavior that is easy to miss

- `/list` does **synchronous** fetch on cache miss; if fetch fails, it still returns `[]`.
- If cached data is stale, `/list` returns stale rows and schedules async refresh (`stale_refresh`).
- `WINTER` requests always attempt prior-year backfill (`year-1`) and merge prior-year `DEC` starts.
- If resolver is not loaded, `/list` returns 503 and `/health` returns 503 (`degraded`).
- `/cache/stats` and `/cache/clear` are debug endpoints gated by `DEBUG_ENDPOINTS_ENABLED`; `ADMIN_TOKEN` enables bearer auth.
- Docker image starts `/server` directly as a non-root distroless process; `/data` must be readable and writable by the runtime UID/GID.
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
- `make check` runs native local checks without containers; `make check-images` explicitly adds Docker validation. CI retains container checks.
- Native regression: `make regression` or `./testdata/native-regression.sh`. Default data is current year minus one plus prior December; captures once, then replays identical inputs against release/work binaries.
- Regression options: `--ref-tag` (`NATIVE_REF_TAG`), `--ref-binary`, `--work-binary`, `--snapshot`, `--year`, `--output`, `--requests`. Supplied binaries plus a snapshot support offline replay. Every run retains artifacts and reports its path.
- Deterministic pipeline matrix: `go test -race -run TestHandleListPipelineMatrix ./cmd/server`.
- Warm-path benchmark: `go test -run '^$' -bench BenchmarkListHit -benchmem ./cmd/server`.

## Release/workflow expectations

- Release Please owns routine versions and `CHANGELOG.md`; do not edit them manually.
- The trusted `Release` workflow publishes the exact created tag to GHCR for Linux amd64/arm64.
- Pull requests use Conventional Commit titles; the stable required check is `Required`.
- Full CI runs on normal PRs, merge groups, and manual runs, not main pushes. Trusted metadata-only release PRs keep metadata validation and CodeQL; `scripts/ci_scope.py` validates intentional skips.
- `scripts/release_scope.py` requires changed application/image inputs since the highest published stable tag before obtaining release credentials. Extend its inputs if embedded assets or new image inputs are added.
- Release credentials are `RELEASE_APP_CLIENT_ID` and `RELEASE_APP_PRIVATE_KEY`; they never belong in PR workflows.
- See `docs/CI_RELEASES.md` for first-release verification, failed-publication recovery, and rollback.

## Rootless acceptance

- Conventional Compose retains its existing bind/default UID and refuses missing host sources. Rootless Docker uses the explicit named volume and image UID 65532. Podman uses host-owned binds with keep-id mapping to 65532:65532.
- Recipes use read-only roots, cap-drop ALL, no-new-privileges, and 30-second container stop grace; Quadlet gives systemd 45 seconds. No scratch tmpfs is needed by the verified paths.
- Signal cancellation includes startup, prewarm, request BaseContext, and background work. Shutdown allows five seconds for prewarm, ten for HTTP drain, five for scheduler wait, then synchronously closes cache. Cache-close failure changes the service result; success logging occurs afterward.
- `testdata/runtime-smoke.sh --engine docker|podman [--rootless] IMAGE` uses controlled loopback mapping/list fixtures and disposable mounts. `testdata/rootless-smoke.sh ENGINE IMAGE [ARCHIVE INSPECTION]` starts isolated engines; CI provisioning is separate and never runs tested containers with sudo.
- Native amd64/arm64 CI imports identical candidate config/layers into both rootless engines. Publication pulls exact platform digests and both engine scenarios must pass before promotion. Test helpers under testdata remain outside production context and release eligibility.
- Production media-stack source and activation have independent review/acceptance gates; disposable evidence does not establish live acceptance. Do not merge release proposals or activate production from this work alone.
