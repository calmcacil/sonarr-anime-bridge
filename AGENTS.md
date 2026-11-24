# Repository guide

`cmd/server` serves Sonarr lists. `internal/anilist` fetches complete years; `internal/cache` stores one SQLite row per year; `internal/scheduler` coordinates fetches and refreshes. `internal/filter` filters candidates and `internal/mapping` resolves IDs against an atomically published snapshot. See [request lifecycle](docs/flow.md).

Use the pinned tools in `mise.toml`. [Development](docs/DEVELOPMENT.md) lists tasks and [preflight](docs/PREFLIGHT_TEST.md) describes native replay. For performance work, read `BENCHMARKS.local.md` and `.local.md` when present; preserve ignored evidence and use separate output directories.

Keep debug endpoints gated. Preserve synchronous primary misses, stale serving, winter backfill and bounded shutdown. Rootless recipes use engine-specific storage and identity; do not infer production acceptance from disposable checks.

Release Please owns versions and `CHANGELOG.md`. If production build inputs change, update the Docker context and `scripts/release_scope.py` together. Release credentials never belong in PR workflows. See [CI and releases](docs/CI_RELEASES.md).
