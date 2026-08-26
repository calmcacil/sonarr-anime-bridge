# Sonarr AniList Bridge

Sonarr-compatible seasonal anime lists from AniList, served as a Docker container
with a built-in HTTP server and SQLite year cache.

## Support status

This repository is maintained. The supported artifact is the rootless OCI image
for Linux `amd64` and `arm64`, published to GHCR. The supported Sonarr entry
point is the Custom List URL shown below; `/health` is available for deployment
checks. See the [operations runbook](docs/OPERATIONS.md) for troubleshooting.

## Prerequisites

- Docker Engine with the Compose v2 plugin (`docker compose`)
- A host or Docker runtime supporting Linux `amd64` or `arm64`
- Outbound HTTPS access to AniList and the configured mapping release host
- A host directory that the container runtime UID/GID can read and write

## Quick start

```bash
mkdir -p ./appdata/sonarr-anime-bridge
chown -R "${PUID:-1000}:${PGID:-1000}" ./appdata/sonarr-anime-bridge
docker compose up -d
```

Point Sonarr at `http://localhost:8080/list?season=all&year=2026`.

## Usage

Add a **Custom List** in Sonarr:

```text
http://<host>:8080/list?season=all&year=2026
```

### Query parameters

| Param | Values | Default |
|-------|--------|---------|
| `season` | `WINTER`, `SPRING`, `SUMMER`, `FALL`, `all` | `all` |
| `year` | Any year within `current year ± 10`; otherwise `400` | current year |
| `category` | `series`, `series-new` (excludes prequels/parents) | `series` |

The HTTP listener starts before prewarm finishes. If the requested year is in
`PREWARM_YEARS`, prewarm normally populates it during startup; otherwise the
first request performs a synchronous fetch and returns populated data, or `[]` if
the fetch fails.

For `WINTER` season, if the prior year is not yet cached, the request triggers
a background fetch for that prior year. The response includes December-starting
shows from the prior year's winter season once both years are cached.

## Configuration

All via environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `8080` | HTTP listen port |
| `PREWARM_YEARS` | current year | CSV years to fetch at startup |
| `INCLUDE_TYPES` | `TV,ONA` | AniList formats: `TV`, `ONA`, `TV_SHORT`, `OVA`, `SPECIAL`, `MOVIE`, `MUSIC` |
| `EXCLUDE_TAGS` | — | CSV AniList tags to exclude |
| `FILTER_FUTURE_ENABLED` | `true` | Drop shows >3 months in the future |
| `MAPPING_PATH` | `/data/anibridge_mappings.json.zst` | Cached anibridge mapping file |
| `MAPPING_URL` | GitHub release URL | Upstream anibridge mapping source |
| `CACHE_DB_PATH` | `/data/cache.db` | SQLite file path |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DEBUG_ENDPOINTS_ENABLED` | `false` | Enable `/cache/stats` and `/cache/clear` |
| `ADMIN_TOKEN` | — | Bearer token required for debug endpoints when set |

### Docker appdata ownership

The image uses a rootless distroless runtime and starts `/server` directly as a
non-root user. It does not include a shell, package manager, `su-exec`, or
startup `chown` logic.

`/data` is the persistent writable path for the cache database, SQLite WAL files,
mapping cache, and mapping metadata. For bind mounts, make the host appdata
directory writable by the runtime UID/GID before starting the container:

```bash
mkdir -p ./appdata/sonarr-anime-bridge
chown -R "${PUID:-1000}:${PGID:-1000}" ./appdata/sonarr-anime-bridge
```

The sample Compose file uses `user: "${PUID:-1000}:${PGID:-1000}"`. `PUID` and
`PGID` are Docker/Compose variables only; they are not application environment
variables.

On startup, the service checks the parent directories for `CACHE_DB_PATH` and
`MAPPING_PATH`. If either directory does not exist, is not a directory, or is not
readable and writable by the runtime user, startup stops before opening SQLite
or downloading mappings.

### Docker image tags and updates

Published images are immutable for a given digest: the container does not
download or replace the application binary at startup. This keeps rollbacks,
debugging, SBOM/provenance, offline startup, and repeated deployments
predictable.

Choose an image reference based on how much automatic movement you want:

| Image reference | Meaning | Use when |
|---|---|---|
| `ghcr.io/calmcacil/sonarr-anime-bridge:latest` | Newest stable release; mutable | You want the simplest update path |
| `ghcr.io/calmcacil/sonarr-anime-bridge:v2` | Latest compatible v2 release; mutable | You want major-line updates |
| `ghcr.io/calmcacil/sonarr-anime-bridge:v2.13` | Latest v2.13 patch; mutable | You want patch updates only |
| `ghcr.io/calmcacil/sonarr-anime-bridge:v2.13.0` | Exact release tag | You want reproducible deploys and easy rollback |
| `ghcr.io/calmcacil/sonarr-anime-bridge@sha256:<digest>` | Exact image digest | You want maximum reproducibility |

The sample Compose file uses the `v2` major track:

```yaml
services:
  sonarr-seasonal:
    image: ghcr.io/calmcacil/sonarr-anime-bridge:v2
    user: "${PUID:-1000}:${PGID:-1000}"
    volumes:
      - "${APPDATA_DIR:-./appdata/sonarr-anime-bridge}:/data"
```

For a pinned deployment, use an exact release tag:

```yaml
image: ghcr.io/calmcacil/sonarr-anime-bridge:v2.13.0
```

Recommended update approaches are external to the container:

- Manual: `docker compose pull && docker compose up -d`
- Watchtower
- Renovate or Dependabot for Compose/GitOps repositories
- GitOps or other deployment automation

To roll back, set `image:` to the previous exact release tag, then run:

```bash
docker compose pull
docker compose up -d
```

### Hardcoded values

The following operational parameters have fixed defaults:

- **HTTP timeout**: 30s (AniList API requests)
- **Docker healthcheck**: `GET /health` via `/server --healthcheck`
- **Winter overflow**: December-starting shows from prior year merged automatically for `season=WINTER`
- **Future filter**: 3 months ahead (when `FILTER_FUTURE_ENABLED=true`)
- **Cache refresh**: current year daily, past years weekly
- **Cache eviction**: 14 days since last access
- **Mapping refresh**: daily (24h)
- **Allowed request methods**: `GET`/`HEAD` for `/list`, `/health`, `/cache/stats`; `POST` for `/cache/clear`

## How it works

1. **Startup**: Server loads the anibridge mapping database, starts listening,
   then prewarms configured years.
2. **`/list`**: Sonarr hits the endpoint → checks SQLite year cache.
3. **Cache hit**: Reads raw AniList JSON, filters on-the-fly (season, format,
duration, tags, future dates), resolves MAL/AniList IDs to TVDB IDs via the
in-memory anibridge mapping, and returns the JSON array.
4. **Cache miss** (non-prewarmed year): Synchronously fetches the year; returns `[]` if fetch fails.
5. **Backfill**: Fetches all anime for that year from AniList GraphQL (single
   paginated query) → stores raw response in SQLite.
6. **Background scheduler**: Refreshes stale year entries (daily for current
   year, weekly for past), prunes entries not requested in 14 days, and checks
   for upstream mapping updates every 24h.
7. **Health check**: `GET`/`HEAD /health` returns component JSON with top-level `status` plus `checks.cache.status` (`ok`, `warming`, or `unhealthy`) and `checks.resolver.status` (`ok` or `degraded`). Cache `warming` is informational: a loaded resolver still produces aggregate HTTP `200` and top-level `status: "ok"`; an unloaded resolver or unreachable cache produces HTTP `503`.
8. **Debug**: `/cache/stats` returns cache hit/miss/entry counts only when debug endpoints are enabled.
9. **Clear**: `POST /cache/clear` wipes all cached data when debug endpoints are enabled.

For health interpretation, degraded-state diagnosis, cache behavior, debug endpoints, structured logs, and `/data` troubleshooting, see the [operations runbook](docs/OPERATIONS.md).

Since filtering and TVDB resolution happen on-the-fly per request, mapping
updates take effect immediately without re-fetching AniList data, and config
changes (format types, tag exclusions, future filtering) apply on restart.

## Path and URL validation

- `CACHE_DB_PATH` and `MAPPING_PATH` must be plain absolute filesystem paths
  (defaults point under `/data`).
- Their parent directories must already exist and be readable and writable by
  the runtime user.
- `MAPPING_URL` must use HTTPS and resolve to an allowlisted anibridge release
  host; unsafe or unknown hosts fall back to the default.

## Building

```bash
go build ./cmd/server
```

Run the complete local CI equivalent with `make check`. Release Please maintains
the reviewed version/changelog pull request; after it creates an immutable tag,
the release workflow builds and publishes the multi-architecture image to
`ghcr.io`. See [CI and releases](docs/CI_RELEASES.md) for setup, verification,
recovery, and rollback.

For deeper preflight commands, see the [Preflight test plan](docs/PREFLIGHT_TEST.md).

## History

This project was extracted from [`calmcacil/sonarr-seasonal-lists`](https://github.com/calmcacil/sonarr-seasonal-lists)
and supersedes the archived [`calmcacil/sonarr-anime-lists`](https://github.com/calmcacil/sonarr-anime-lists)
(replaced `shinkro/community-mapping` YAML with `anibridge/anibridge-mappings`,
adding AniList→TVDB resolution for ~9,100 additional entries and recovering
previously unresolvable shows).

## Licenses

| Document | Contents |
|---|---|
| [LICENSE](./LICENSE) | MIT License |
| [NOTICE](./NOTICE) | Third-party attribution |
