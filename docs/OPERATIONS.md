# Operations Runbook

This runbook covers common degraded states, cache behavior, debug endpoints, structured logs, and persistent-data failures. Configuration defaults and deployment examples remain in the [README](../README.md).

## First checks

1. Check container state and recent logs:

   ```sh
   docker compose ps
   docker compose logs --tail=200 sonarr-seasonal
   ```

2. Query health without exposing configuration or credentials:

   ```sh
   curl -i http://127.0.0.1:8080/health
   ```

3. Confirm the host directory mounted at `/data` is readable and writable by the UID/GID configured in Compose:

   ```sh
   stat ./appdata/sonarr-anime-bridge
   ```

Do not solve permission failures with world-writable modes. Set ownership to the configured runtime UID/GID and grant only the access the deployment needs.

## Health states

`GET /health` and `HEAD /health` are unauthenticated. The response exposes finite component states only; it does not include tokens, paths, upstream URLs, configured years, query details, identifiers, or raw errors.

A ready service returns HTTP `200`:

```json
{
  "status": "ok",
  "checks": {
    "cache": { "status": "ok" },
    "resolver": { "status": "ok" }
  }
}
```

While startup prewarm is still running, the aggregate remains healthy:

```json
{
  "status": "ok",
  "checks": {
    "cache": { "status": "warming" },
    "resolver": { "status": "ok" }
  }
}
```

If no mapping is loaded, the service is degraded:

```json
{
  "status": "degraded",
  "reason": "resolver not loaded",
  "checks": {
    "cache": { "status": "ok" },
    "resolver": { "status": "degraded" }
  }
}
```

If SQLite cannot be queried, the service is unhealthy:

```json
{
  "status": "unhealthy",
  "checks": {
    "cache": { "status": "unhealthy" },
    "resolver": { "status": "ok" }
  }
}
```

Component meanings:

| Component | Status | Meaning | Aggregate effect |
|---|---|---|---|
| `cache` | `ok` | SQLite is reachable and every configured prewarm year has a cached row. | None |
| `cache` | `warming` | SQLite is reachable, but at least one configured prewarm year has no row yet. | Informational; remains HTTP `200` when the resolver is loaded. |
| `cache` | `unhealthy` | The health request cannot reach SQLite. | HTTP `503`, top-level `unhealthy`. |
| `resolver` | `ok` | Anibridge mappings are loaded. | None |
| `resolver` | `degraded` | No mapping is loaded. | HTTP `503`, top-level `degraded`. |

A stale cache row still counts as available. `warming` describes whether configured prewarm rows exist, not whether every possible request year is cached or whether cached upstream content is fresh.

The image healthcheck runs `/server --healthcheck`, calls the local `/health` endpoint, and succeeds only on HTTP `200`. It relies on the aggregate status code rather than parsing component JSON.

## Resolver degraded state

At startup the service tries to load the cached anibridge mapping or fetch it from the configured upstream. If that attempt fails:

- The HTTP listener still starts.
- `/health` returns HTTP `503` with top-level `degraded` and resolver `degraded`.
- `/list` returns HTTP `503` because unresolved identifiers must not be presented as a valid Sonarr list.
- The background scheduler retries mapping load every minute while no resolver is loaded.

Check logs with `type=resolver`. An initial failure is logged as a failed mapping load. A successful retry logs mapping load/refresh information and health becomes ready without a restart.

After a resolver has loaded, it remains active while the service checks for an updated mapping every 24 hours. A later refresh failure logs `anibridge mapping refresh failed, keeping current mapping`; it does not discard the working resolver or degrade health.

Actions:

1. Confirm outbound HTTPS and DNS access to the configured mapping host.
2. Confirm the mapping parent directory is readable and writable by the runtime UID/GID.
3. Check whether a cached mapping and its metadata are readable.
4. Allow the one-minute unloaded retry to recover after fixing the cause; restart only when deployment configuration changed.

## Cache and list behavior

The cache stores one raw AniList response row per year. Filtering and TVDB resolution happen when `/list` is requested.

### Cache miss

A request for a year with no cache row performs a synchronous AniList fetch. A successful fetch stores the raw year and returns the processed list. If the fetch fails, `/list` logs the error and returns `[]`; inspect `type=fetch` and HTTP logs rather than treating an empty array alone as proof that no anime exists.

### Stale row

Current-year rows become stale after one day; past-year rows become stale after seven days. A request returns stale data immediately and schedules a non-blocking refresh. A refresh failure leaves the stale row available.

### Winter overflow

`season=WINTER` also considers December-starting shows from the prior year. If the prior-year row is absent, the request starts a non-blocking background fetch for that year and continues with the current year's data; the response may temporarily omit prior-December entries until a later request observes the backfill. If prior-year data is already cached, the response can use that row (including a stale row) without waiting for a refresh. Duplicate AniList IDs are removed when the two years are merged.

### Startup prewarm

The listener starts before `PREWARM_YEARS` prewarm completes. Health can therefore report cache `warming` while returning HTTP `200`. A prewarm failure does not stop the server; later `/list` requests can retry through normal cache-miss behavior.

## Debug endpoints

Debug endpoints are disabled unless `DEBUG_ENDPOINTS_ENABLED=true`.

| Endpoint | Method | Purpose |
|---|---|---|
| `/cache/stats` | `GET` or `HEAD` | Return cache entry, hit, and miss counts. |
| `/cache/clear` | `POST` | Delete cached year data and reset cache counters. |

When `ADMIN_TOKEN` is empty, enabled debug endpoints require no bearer token. When it is set, send it only in the `Authorization` header:

```sh
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:8080/cache/stats

curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:8080/cache/clear
```

For valid endpoint methods, disabled or unauthorized debug requests return not found. Unsupported methods still return `405` with the endpoint's `Allow` header. A `GET /cache/clear` request is not supported.

Never put `ADMIN_TOKEN` in a URL, command history literal, Compose log output, or support bundle. Prefer an environment variable or secret manager and redact authorization headers before sharing diagnostics.

## Structured logs

Logs are structured JSON in normal container operation. Not every event includes every field.

| Field | Meaning |
|---|---|
| `type` | Subsystem or event family, such as `system`, `http`, `resolver`, `cache`, `fetch`, `filter`, or `scheduler`. |
| `category` | Requested list category where relevant. |
| `trigger` | Why work started, such as `cache_miss`, `prewarm`, `stale_refresh`, or `winter_overflow`. |
| `year` | Anime/cache year involved in the event. |
| `season` | Normalized requested season. |
| `duration_ms` | Completed operation duration in milliseconds. |
| `status` | HTTP response status or operation status where emitted. |
| `method` | HTTP request method. |
| `path` | HTTP request path; do not infer query values from it. |
| `entries` | Number of cached year rows. |
| `hits` | Cache-hit counter. |
| `misses` | Cache-miss counter. |
| `error` | Server-side error detail; review before sharing because host/runtime messages can contain deployment context. |

Useful filters depend on the container log backend. With plain Docker output:

```sh
docker compose logs sonarr-seasonal | jq 'select(.type == "resolver")'
docker compose logs sonarr-seasonal | jq 'select(.type == "fetch")'
```

## Persistent `/data` failures

The service validates the parent directories for `CACHE_DB_PATH` and `MAPPING_PATH` before opening SQLite or downloading mappings. Startup exits when a directory:

- does not exist;
- is not a directory;
- cannot be listed by the runtime user;
- cannot create, write, close, or remove a temporary probe file.

The standard image expects `/data` to hold the SQLite database and sidecars, mapping data, and mapping metadata. The container does not repair bind-mount ownership.

For the default Compose identity:

```sh
mkdir -p ./appdata/sonarr-anime-bridge
chown -R "${PUID:-1000}:${PGID:-1000}" ./appdata/sonarr-anime-bridge
docker compose up -d
```

If startup reports runtime data-directory validation failure:

1. Compare the Compose `user:` UID/GID with host ownership.
2. Confirm the mount source exists and the destination is `/data`.
3. Confirm no regular file is mounted where a directory is expected.
4. Correct ownership or the deployment mount, then recreate the container.
5. Do not delete the database or mapping files unless logs identify data corruption and a backup/recovery decision has been made.

## Updates and rollback

Before an update, record the current exact tag or digest and retain `/data`. After recreating the service:

```sh
docker compose ps
docker compose logs --tail=200 sonarr-seasonal
curl -fsS http://127.0.0.1:8080/health | jq .
```

Confirm resolver `ok`; cache may briefly report `warming`. For rollback, select the previous exact tag or digest, run `docker compose pull && docker compose up -d`, and repeat the same checks. Container rollback does not mutate or self-update the application binary.

For release workflow and publication recovery, see [CI and releases](CI_RELEASES.md). For deeper preflight commands, see [Preflight test plan](PREFLIGHT_TEST.md).
