# Sonarr AniList Bridge

Serve seasonal AniList anime as a Sonarr Custom List of TVDB series. The image supports Linux amd64 and arm64. It needs writable persistent storage at `/data` and outbound HTTPS for AniList and anibridge mappings.

## Quick start

On rootless Docker, start the [v3 Compose recipe](examples/docker-compose.rootless.yml):

```sh
docker info --format '{{json .SecurityOptions}}'  # Check for name=rootless.
docker compose -f examples/docker-compose.rootless.yml up -d
curl -fsS http://localhost:8080/health
```

In Sonarr, add a **Custom List** pointing to `http://<bridge-host>:8080/list`. Use a hostname reachable from Sonarr; `localhost` inside a container points to that container.

## List options

| Param | Values | Default |
|-------|--------|---------|
| `season` | `WINTER`, `SPRING`, `SUMMER`, `FALL`, `all` | `all` |
| `year` | Any year within `current year ± 10`; otherwise `400` | current year |
| `category` | `series`, `series-new` (excludes prequels/parents) | `series` |
| `country` | CSV AniList origin codes (`JP`, `CN`, `KR`, etc.) or `ALL` | `DEFAULT_COUNTRIES` |

For example, `/list?season=WINTER&country=JP` returns Japanese winter titles, including eligible December starts from the previous year once cached. Country filtering requires v3.1.0 or later. An explicit `country` replaces the instance default; `ALL` disables it. Unknown origins are excluded only when filtering. Blank, repeated or malformed country parameters return HTTP 400.

## Configuration

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `8080` | HTTP listen port |
| `PREWARM_YEARS` | current year | CSV years to fetch at startup |
| `INCLUDE_TYPES` | `TV,ONA` | AniList formats: `TV`, `ONA`, `TV_SHORT`, `OVA`, `SPECIAL`, `MOVIE`, `MUSIC` |
| `DEFAULT_COUNTRIES` | unset (all origins) | CSV AniList country-of-origin codes; overridden by `/list?country=…`. Invalid values stop startup. |
| `EXCLUDE_TAGS` | — | CSV AniList tags to exclude |
| `FILTER_FUTURE_ENABLED` | `true` | Drop shows >3 months in the future |
| `MAPPING_PATH` | `/data/anibridge_mappings.json.zst` | Cached anibridge mapping file |
| `MAPPING_URL` | GitHub release URL | Upstream anibridge mapping source |
| `CACHE_DB_PATH` | `/data/cache.db` | SQLite file path |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DEBUG_ENDPOINTS_ENABLED` | `false` | Enable `/cache/stats` and `/cache/clear` |
| `ADMIN_TOKEN` | — | Bearer token required for debug endpoints when set |

Cache and mapping paths must be absolute, with existing readable and writable parent directories. Mapping downloads require an allowlisted HTTPS host.

## Rootless deployment recipes

| Recipe | Engine and process identity | Persistent storage |
| --- | --- | --- |
| [Conventional Compose](docker-compose.yml) | Conventional Docker; explicit `${PUID:-1000}:${PGID:-1000}` | Existing `${APPDATA_DIR:-./appdata/sonarr-anime-bridge}` bind; directory must already exist and be writable |
| [Rootless Compose](examples/docker-compose.rootless.yml) | Rootless Docker; image user 65532:65532 | Explicit `sonarr-anime-bridge-rootless-data` volume; initialized from image ownership |
| [Portable Quadlet](examples/sonarr-anime-bridge.container) | Rootless Podman; host account mapped to 65532:65532 by keep-id | Private `%h/.local/share/sonarr-anime-bridge` host-owned bind |

Conventional Compose intentionally follows `v2`; its default image does not include country filtering. Review [storage migration and rollback](docs/OPERATIONS.md#rootless-migration-and-rollback) before switching tracks. For repeatable deployments pin a full release tag or digest. Do not remove the data volume when recreating a container.

## Maintainer references

[Operations](docs/OPERATIONS.md) · [Development](docs/DEVELOPMENT.md) · [CI and releases](docs/CI_RELEASES.md) · [Security](SECURITY.md) · [License](LICENSE) · [Third-party notices](NOTICE)
