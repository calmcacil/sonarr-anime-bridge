# Operations

```sh
curl -sS -i http://localhost:8080/health
docker compose logs --tail=200 sonarr-seasonal
```

`/health` returns 200 if SQLite is reachable and the resolver is loaded. Cache `warming` is informational. A degraded resolver or unhealthy cache returns 503. If mappings cannot load, check outbound HTTPS and the mapping path; `/list` cannot resolve titles until the resolver recovers.

A cache miss waits for AniList; failure can return HTTP 200 with `[]`. A stale year remains usable while it refreshes. Winter results can grow after prior-year backfill. Do not clear the database to enable country filtering: restricted requests refresh legacy rows automatically.

## Structured logs

JSON logs go to stderr. Check task outcome, trigger, duration and failure stage to distinguish an empty result from a failed fetch. Use `LOG_LEVEL=debug` for page, filter and mapping detail.

## Debug endpoints

With `DEBUG_ENDPOINTS_ENABLED=true`, `GET /cache/stats` reports cache counters and `POST /cache/clear` clears cached years. If `ADMIN_TOKEN` is set, send `Authorization: Bearer <token>`. Keep these endpoints private. Clearing forces new upstream fetches; it is not a first-line recovery step.

## Rootless migration and rollback

Record the current image digest, engine, identity and mount. Stop the old writer and back up `/data` consistently. Never run two writers on one mount. A new named volume selects different data; changing the image does not migrate or restore storage.

Use the [recipe for your engine](../README.md#rootless-deployment-recipes). Conventional binds must already exist and be readable and writable; Podman keep-id maps the image user to the invoking account. Rootless Docker uses a named volume. Do not recursively change ownership or delete a volume to fix startup.

After recreation, check health, a representative list, persistence and clean shutdown. To roll back, restore the previous image, identity and mount; restore data only from a verified backup. A healthcheck reports readiness, not a restart trigger.
