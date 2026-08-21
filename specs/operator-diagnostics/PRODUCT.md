# Product Spec: Operator Diagnostics and Runtime Smoke Coverage

## Summary

Sonarr Anime Bridge operators need a concise runbook, a more diagnostic but still safe `/health` response, and automated container smoke coverage for writable and unwritable `/data` mounts. These additions improve diagnosis and deployment confidence without changing list generation, cache refresh, mapping, or resolver behavior.

## Goals

- Let operators diagnose common resolver, cache, debug-endpoint, and structured-log states without reading source code.
- Expose component-level health details that are safe for an unauthenticated endpoint.
- Detect container startup and `/data` permission regressions in automated runtime smoke coverage.

## Non-goals

- Change `/list`, cache refresh, mapping refresh, resolver retry, filtering, or debug-endpoint behavior.
- Turn `/health` into an authenticated diagnostics endpoint or expose configuration values.
- Make initial cache prewarming a prerequisite for HTTP health or container readiness.
- Repair host bind-mount ownership or permissions from inside the container.

## Behavior

### Operator runbook

1. The repository provides a focused operator runbook at `docs/OPERATIONS.md`, linked from the primary README so an operator can discover it without searching the source tree.

2. The runbook explains the resolver lifecycle in operational terms:
   - The service attempts to load mappings during startup.
   - A failed initial load leaves the service listening in a degraded state.
   - `/list` remains unavailable while the resolver is unloaded.
   - The background resolver retry runs every minute while unloaded.
   - Once loaded, the active resolver remains usable if a later daily refresh fails.
   - The runbook identifies the health response and structured resolver logs an operator should use to distinguish unloaded, recovered, and refresh-failed states.

3. The runbook explains cache request behavior:
   - A cache miss for the requested year performs a synchronous upstream fetch before responding; the special prior-year `WINTER` backfill is non-blocking as described below.
   - A failed cache-miss fetch returns an empty list rather than an HTTP upstream error.
   - Stale cached data is returned immediately and refreshed asynchronously.
   - A `WINTER` request with no prior-year cache schedules a non-blocking background backfill for prior-year December data, so the current response may omit December entries. If prior-year data is cached but stale, it remains immediately usable while the asynchronous refresh runs.
   - Initial configured-year prewarming begins after the HTTP listener starts and does not determine the overall health status.

4. The runbook documents `/cache/stats` and `POST /cache/clear`, including their allowed methods, the `DEBUG_ENDPOINTS_ENABLED` gate, and bearer authentication when `ADMIN_TOKEN` is set. It explicitly states that disabled or unauthorized debug endpoints return the existing not-found response and that the admin token must never be placed in a URL or logs.

5. The runbook provides a concise field reference for common structured logs. At minimum it explains `type`, `category`, `trigger`, `year`, `season`, `duration_ms`, `status`, `method`, `path`, `entries`, `hits`, `misses`, and `error`, while noting that not every event includes every field.

6. The runbook maps common symptoms to concrete checks and actions, including:
   - `/health` reports an unloaded resolver.
   - Cache readiness reports that prewarming is incomplete.
   - `/list` returns an empty array after an upstream failure.
   - Debug endpoints return not found.
   - Startup exits because `/data` is absent, not a directory, unreadable, or unwritable.
   - A previously healthy resolver logs a refresh failure.

7. Runbook guidance does not instruct operators to expose tokens, weaken filesystem permissions globally, delete persistent data as a first response, or depend on undocumented application internals.

### Health response

8. `GET` and `HEAD /health` retain their existing method support, JSON content type, and HTTP status semantics. Unsupported methods continue to return `405` with `Allow: GET, HEAD`.

9. Every `/health` JSON response includes:
   - The existing top-level `status` value.
   - A `checks` object with named `cache` and `resolver` members.
   - Each named member is an object containing a `status` string with a stable, documented value rather than free-form error text.

   A healthy response with complete prewarm data has this shape:

   ```json
   {
     "status": "ok",
     "checks": {
       "cache": { "status": "ok" },
       "resolver": { "status": "ok" }
     }
   }
   ```

10. The resolver check reports `ok` when mappings are loaded and `degraded` when they are not loaded. An unloaded resolver continues to make the top-level status `degraded` and the response HTTP `503`; a loaded resolver does not expose mapping counts, mapping paths, upstream URLs, retry timestamps, or errors.

11. The cache check distinguishes three observable conditions:
   - `ok`: SQLite is reachable and every configured prewarm year has a cached row.
   - `warming`: SQLite is reachable but one or more configured prewarm years do not yet have a cached row.
   - `unhealthy`: SQLite cannot be reached for the health request.

12. With zero configured prewarm years, no cached rows are required, but SQLite must still be reachable; a failed reachability probe yields `unhealthy`. A cached row counts as ready regardless of whether it is fresh or stale. Cache readiness describes data availability for configured prewarm years; it does not claim that upstream content is current or that every request year is cached.

13. Cache `warming` is informational. It does not change the top-level health status or HTTP code: when SQLite is reachable and the resolver is loaded, `/health` returns HTTP `200` with top-level `status: "ok"` even while the cache check is `warming`.

14. Cache `unhealthy` continues to make the top-level status `unhealthy` and the response HTTP `503`. Resolver state is still reported in `checks` so the response describes both components without replacing the aggregate status.

15. The existing resolver-degraded reason may remain for backward compatibility, but clients must be able to diagnose component state from `checks`. No new response field contains secrets, tokens, filesystem paths, mapping URLs, query parameters, anime titles or identifiers, configured years, raw errors, or other request-specific data.

16. A `HEAD /health` request has the same status code and headers as the equivalent `GET` request and no response body, consistent with HTTP semantics.

17. The health extension is additive. Existing clients that use only the HTTP code or top-level `status` continue to receive the same healthy, degraded, and unhealthy decisions as before this feature.

### Docker/runtime smoke coverage

18. Automated pull-request coverage builds the supported runtime image and starts it as a non-root runtime user with a temporary writable host directory mounted at `/data`.

19. The writable-mount smoke scenario waits for the container healthcheck within a bounded timeout and fails with useful container logs if the service exits, remains unhealthy, or times out.

20. The writable-mount scenario verifies that startup creates and uses its persistent runtime files under `/data`, that `GET /health` returns the documented structured JSON shape, and that the image healthcheck agrees with the endpoint's HTTP status.

21. The runtime smoke does not depend on mutable production data or unbounded live upstream behavior. Any mappings or upstream responses needed to reach the asserted state are controlled by the smoke environment so the result is repeatable.

22. A second smoke scenario starts the same image and runtime UID/GID with an existing `/data` mount that the runtime user cannot write.

23. The unwritable-mount scenario passes only when the container exits non-zero before serving requests and reports a clear runtime data-directory validation failure. It fails if the service continues running, becomes healthy, or silently falls back to non-persistent storage.

24. The unwritable-mount scenario confirms that startup does not create the SQLite database, SQLite sidecars, mapping data, mapping metadata, or leftover write-probe files in the rejected mount.

25. Both smoke scenarios clean up containers, networks, temporary files, and temporary directories after success or failure so reruns are isolated.

26. Runtime smoke coverage complements rather than replaces Go tests, static checks, supported-architecture builds, or the existing image version smoke test.

27. Existing `/list`, cache, mapping, resolver, filtering, and debug-endpoint behavior remains unchanged except for the additive `/health` JSON fields defined in this specification.
