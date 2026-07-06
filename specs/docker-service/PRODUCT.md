# Product Spec: Dockerized Sonarr Seasonal Lists Service

A long-running Go HTTP service packaged as a multi-arch Docker image that:

1. Serves Sonarr-compatible seasonal anime list JSON at `/list`
2. Caches raw AniList JSON per year in SQLite
3. Resolves AniList IDs to TVDB IDs via `anibridge/anibridge-mappings`
4. Filters on-the-fly per request (season, format, duration, tags, future-date, first-season)
5. Attempts synchronous fetch on cache miss; returns `[]` if fetch fails
6. Refreshes stale data (current year daily, past years weekly), prunes cold entries (14 days)
7. Refreshes the anibridge mapping daily via conditional HTTP (ETag)

## Endpoints

| Endpoint | Methods | Purpose |
|----------|---------|---------|
| `/list` | `GET`, `HEAD` | Sonarr import list |
| `/health` | `GET`, `HEAD` | Liveness check |
| `/cache/stats` | `GET`, `HEAD` | Cache debug stats (entries, hits, misses). Disabled unless `DEBUG_ENDPOINTS_ENABLED=true` |
| `/cache/clear` | `POST` | Wipe all cached data. Disabled unless `DEBUG_ENDPOINTS_ENABLED=true` |

### `/list` query parameters

| Param | Values | Default |
|-------|--------|---------|
| `season` | `WINTER`, `SPRING`, `SUMMER`, `FALL`, `all` | `all` |
| `year` | Any year within `current year ± 10`; otherwise `400` | current year |
| `category` | `series`, `series-new` (excludes prequels/parents) | `series` |

### Expected behavior

| Scenario | Behavior |
|----------|----------|
| Prewarmed year | Returns populated JSON immediately |
| Non-prewarmed year | Synchronous fetch attempt; returns `[]` if fetch fails |
| Backfill complete | Returns full JSON array |
| Stale cached data | Returns data immediately, schedules async refresh |
| Entry not hit in 14 days | Pruned |
| `WINTER` + prior year uncached | Triggers prior-year backfill in background; merges prior-year December starts |

### Configuration

All via environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `8080` | HTTP listen port |
| `PREWARM_YEARS` | current year | CSV years to fetch at startup |
| `INCLUDE_TYPES` | `TV,ONA` | AniList formats: `TV`, `ONA`, `TV_SHORT`, `OVA`, `SPECIAL`, `MOVIE`, `MUSIC` |
| `EXCLUDE_TAGS` | — | CSV AniList tags to exclude |
| `FILTER_FUTURE_ENABLED` | `true` | Drop shows >3 months in the future |
| `MAPPING_PATH` | `/data/anibridge_mappings.json.zst` | Cached anibridge mapping |
| `MAPPING_URL` | anibridge release URL | Upstream mapping source |
| `CACHE_DB_PATH` | `/data/cache.db` | SQLite path |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DEBUG_ENDPOINTS_ENABLED` | `false` | Require this to expose `/cache/stats` and `/cache/clear` |
| `ADMIN_TOKEN` | — | Optional bearer token required for debug endpoints |

### Docker runtime contract

- The runtime image is rootless distroless and starts `/server` directly.
- `/data` is the only expected persistent writable path in container usage.
- `PUID` and `PGID` are Docker/Compose runtime variables used with `user:`, not application environment variables.
- Bind-mounted appdata must be readable and writable by the selected runtime UID/GID before the container starts.
- Startup fails before opening SQLite or downloading mappings if the cache or mapping parent directory is missing, not a directory, or not readable and writable by the runtime user.
- Published image digests are immutable; the container does not self-update at startup.
- Users choose update behavior with tags: `latest`, major track (`v2`), minor track (`v2.12`), exact release (`v2.12.0`), or digest pinning.
- Updates and rollbacks are driven externally with Compose, Watchtower, Renovate, Dependabot, or GitOps automation.

### Hardcoded values

| Parameter | Value |
|-----------|-------|
| AniList HTTP timeout | 30 s |
| Anibridge HTTP timeout | 60 s |
| AniList page size | 50 |
| Winter overflow | `WINTER` only |
| Future filter window | 3 months |
| Cache freshness: current year | 24 h |
| Cache freshness: past years | 7 days |
| Cache eviction | 14 days |
| Mapping refresh | 24 h |
