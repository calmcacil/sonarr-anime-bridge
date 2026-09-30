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

Check logs with `type=resolver` and `task=mapping_load` or `task=mapping_refresh`. An unloaded failure records `outcome=degraded`, `resolver_loaded=false`, and `retry_in_ms`. A successful retry records `outcome=succeeded` with entry counts; health becomes ready without a restart.

After a resolver has loaded, it remains active while the service checks for an updated mapping every 24 hours. A later refresh failure records `outcome=failed` and `reason=keeping_current_mapping`; it does not discard the working resolver or degrade health.

Actions:

1. Confirm outbound HTTPS and DNS access to the configured mapping host.
2. Confirm the mapping parent directory is readable and writable by the runtime UID/GID.
3. Check whether a cached mapping and its metadata are readable.
4. Allow the one-minute unloaded retry to recover after fixing the cause; restart only when deployment configuration changed.

## Cache and list behavior

The cache stores one raw AniList response row per year. Filtering and TVDB resolution happen when `/list` is requested.

### Cache miss

A request for a year with no cache row performs a synchronous AniList fetch. A successful fetch stores the raw year and returns the processed list. If the fetch fails, `/list` still returns `[]`, but its request event is `WARN` with `outcome=degraded` and `reason=fetch_failed`. The `year_fetch` event owns the upstream error; inspect it rather than treating an empty array alone as proof that no anime exists.

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

The server writes one JSON object per line to stderr. `LOG_LEVEL` selects the
minimum severity (`debug`, `info`, `warn`/`warning`, or `error`; default `info`).
This replaces the previous text output; update text-based log collectors to
parse JSON and select stable fields rather than message wording.

### Task lifecycle

Actual year fetches, mapping loads, prewarm, cache clear, and vacuum announce
`outcome=started`, then report their result with `duration_ms`. Startup reports
listener readiness only after binding the socket. Prewarm runs after the listener
starts, and reports `configured`, `cached`, `fetched`, and `failed` year counts.
Partial prewarm is `degraded`, not success. A fetch success is logged only after
the year is stored in SQLite.

| Outcome | Meaning |
|---|---|
| `started` | Work has started. |
| `succeeded` | The operation completed successfully. |
| `failed` | The operation failed; inspect `stage`, `reason`, and `error`. |
| `degraded` | Work used a fallback, completed partially, or could not provide full service. |
| `canceled` | Context cancellation or a deadline stopped work. |
| `skipped` | No work was performed; `reason` explains why. |
| `waiting` | Work is waiting on an existing fetch or an upstream delay. |
| `retrying` | Another upstream attempt is scheduled after the reported delay. |

INFO shows actual work and successful outcomes. WARN shows recoverable failures,
partial work, upstream waits/retries, and deadline cancellations. ERROR shows
failed year fetches, fatal service failures, and HTTP task errors. Normal shutdown
cancellation is INFO. DEBUG shows fresh-cache/cooldown skips, concurrent-fetch
waits, AniList page progress, parser metadata/key changes, filter counts, and
no-op maintenance. A year-fetch failure is logged by the fetch task once, not
repeated by its prewarm, stale-refresh, or HTTP caller. Aggregate task and request
events still report the effect of that failure.

### Requests and privacy

Completed requests emit one `request completed` event with `task=request`.
Successful `/health` probes are suppressed. HTTP 4xx/5xx, response-write failures,
and recovered panics use WARN completion events. A sent HTTP 200 can therefore
still have `outcome=failed` or `degraded`; status alone is not the task outcome.
A legitimate empty list remains INFO with `outcome=succeeded`.

Request events use matched route patterns, or `unknown`, and never include raw
request URLs, paths, query strings, headers, bearer tokens, or response bodies.
Configuration summaries omit `ADMIN_TOKEN` and `MAPPING_URL`. Mapping decision
events do not expose configured old/new URLs. The `error` field remains detailed
server-side evidence and can include upstream bodies, URLs, or deployment paths;
review and redact it before sharing logs.

### Fields

Not every event includes every field. Use `type`, `task`, and `outcome` to select
events; use `year` and `trigger` to follow year work.

| Field | Meaning |
|---|---|
| `type` | Event family: `system`, `config`, `http`, `resolver`, `cache`, `fetch`, `filter`, `mapping`, or `scheduler`. |
| `task` | Operation: e.g. `startup`, `mapping_load`, `mapping_refresh`, `prewarm`, `year_fetch`, `request`, `show_process`, `cache_clear`, `cache_prune`, or `cache_vacuum`. |
| `outcome` | Task result or progress state from the table above. |
| `trigger` | Origin: e.g. `startup`, `scheduled`, `recovery`, `prewarm`, `cache_miss`, `cache_recovery`, `stale_refresh`, `winter_overflow`, `request`, or `admin`. |
| `stage` | Fetch failure boundary: `cache_check`, `upstream`, `encode`, or `cache_write`; HTTP task stage where available. |
| `reason` | Machine-readable explanation, e.g. `cooldown`, `fresh_cache`, `fetch_failed`, or `keeping_current_mapping`. |
| `duration_ms` | Elapsed operation time in milliseconds. |
| `retry_in_ms` | Fetch cooldown, next mapping check, or sampled AniList exponential retry delay; no retry occurs during shutdown. |
| `wait_ms` | Separate applied AniList `Retry-After` wait; token-bucket throttling can add delay. |
| `attempt`, `next_attempt`, `max_attempts` | Failed upstream attempt, planned next attempt, and request-attempt limit. |
| `retry_after` | AniList header state: `missing`, `invalid`, `valid`, `clamped`, or `not_applicable`. |
| `year`, `season`, `category` | Validated list/cache context. |
| `method`, `route`, `status` | HTTP request method, matched route, and response code; upstream retry `status=0` means a network error. |
| `result_count` | Number of processed list entries. |
| `cache_state` | Initial list cache state: `hit`, `miss`, `stale`, or `invalid`. |
| `refresh_scheduled`, `winter_backfill_scheduled` | Request scheduled non-blocking work; these do not assert that a download completed. |
| `shows`, `page`, `total_shows` | Fetch result count and DEBUG pagination progress. |
| `resolver_loaded`, `mal_entries`, `anilist_entries`, `total_entries` | Mapping availability and entry counts. |
| `configured`, `cached`, `fetched`, `failed` | Aggregate prewarm year counts; canceled prewarm may leave unprocessed years. |
| `entries`, `hits`, `misses` | Cache statistics. |
| `count` | Pruned rows or newly discovered mappings. |
| `source`, `action`, `consequence` | Mapping/retry decision and its operational effect. |
| `error` | Detailed failure evidence; potentially sensitive. |

New mappings emit an INFO aggregate with `task=mapping_discovery`, `count`,
`season`, and `year`. Individual titles and identifiers remain DEBUG-only.
Mapping download fallback is announced only after the cached file parses;
failed HEAD checks instead explain that the service continues with a download.

Example fetch lifecycle (timestamps omitted):

```json
{"level":"INFO","msg":"fetching year","type":"fetch","task":"year_fetch","outcome":"started","year":2026,"trigger":"cache_miss","stage":"upstream"}
{"level":"INFO","msg":"year cached","type":"fetch","task":"year_fetch","outcome":"succeeded","year":2026,"trigger":"cache_miss","stage":"cache_write","shows":320,"duration_ms":8400}
```

With Docker Compose, remove its prefix before piping JSON to `jq`:

```sh
docker compose logs --no-log-prefix sonarr-seasonal | jq 'select(.type == "resolver")'
docker compose logs --no-log-prefix sonarr-seasonal | jq 'select(.task == "year_fetch")'
docker compose logs --no-log-prefix sonarr-seasonal | jq 'select(.outcome == "failed" or .outcome == "degraded")'
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
