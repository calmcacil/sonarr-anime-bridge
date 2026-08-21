# Product Spec: Sonarr Anime Bridge Service Baseline

## Summary

Sonarr Anime Bridge is a long-running HTTP service that produces Sonarr-compatible seasonal anime lists from AniList and anibridge mappings. This specification is the compatibility baseline for the deployed, proven service: future changes must preserve these observable guarantees unless an approved product specification explicitly changes them.

## Goals

- Preserve the stable list, health, cache, configuration, and container contracts.
- Prefer useful cached results during upstream failures without presenting unresolved identifiers as valid lists.
- Remain portable across supported container hosts; no particular production host or deployment values are normative.

## Non-goals

- Guarantee that upstream AniList or anibridge content remains unchanged.
- Guarantee that every AniList title resolves to a TVDB identifier.
- Expose configuration through files, command-line flags, or a management UI.
- Self-update the application binary or repair bind-mount ownership at startup.

## Behavior
<!-- markdownlint-disable MD029 -->

### List API

1. `GET /list` and `HEAD /list` are supported. Other methods return `405 Method Not Allowed` with `Allow: GET, HEAD`.

2. A successful `GET /list` response is JSON with `Content-Type: application/json`. Its body is an array of objects shaped as `{"tvdbId": <integer>, "title": <string>}`. Only shows that resolve to a TVDB ID appear; unresolved shows are omitted.

3. `season` is case-insensitive after surrounding whitespace is removed. Accepted values are `WINTER`, `SPRING`, `SUMMER`, `FALL`, and `all`; omission defaults to `all`. Any other value returns `400 Bad Request`.

4. `year` defaults to the current calendar year. A supplied year must be a positive integer within the current year plus or minus ten years; invalid or out-of-range values return `400 Bad Request`.

5. `category` defaults to `series`, is case-sensitive, and accepts only `series` and `series-new`; any other supplied value returns `400 Bad Request`.

6. Each request filters the cached full-year AniList dataset in this order: requested season, configured formats, per-episode duration greater than ten minutes, configured excluded tags, optional future-date window, and `series-new` relation filtering. Mapping resolution then converts remaining AniList records to output objects.

7. `series-new` excludes entries with an AniList `PREQUEL` or `PARENT` relation. `series` does not apply that relation filter.

8. Excluded-tag matching is case-insensitive. Shows with known per-episode duration of ten minutes or less are excluded; shows with unknown duration are retained.

9. With future filtering enabled, shows whose known start month is more than three months ahead of the request time are excluded. Shows with an unknown start year or month are retained.

10. A show uses its English title when non-empty, then its romaji title, then `Anime #<AniList ID>`.

11. On a cache miss, `/list` synchronously attempts to fetch and store the requested year before responding. A successful fetch is processed in the same request. If the fetch fails or completes without cached data, the service returns HTTP `200` with `[]`.

12. Stale cached data is returned immediately and refreshed asynchronously. A refresh failure does not replace or suppress the stale response.

13. A `WINTER` request also uses December-starting winter entries from the prior calendar year when that year is cached, without duplicating AniList IDs already present in the requested year. If the prior year is absent, the service starts a background fetch; the current response remains valid but may omit those entries until a later request.

14. Prior-year overflow is applied only to `WINTER`, not to `all` or another season.

15. Concurrent fetches for the same year share one in-flight operation rather than issuing duplicate upstream fetches.

16. If no mapping resolver is loaded, `/list` returns `503 Service Unavailable` with `{"status":"degraded","reason":"resolver not loaded"}` rather than returning an empty or unresolved list.

17. Cache access or processing failures return `500 Internal Server Error`. Client cancellation may stop request-scoped work without invalidating already cached data.

### Health and diagnostics

18. `GET /health` and `HEAD /health` are supported. Other methods return `405` with `Allow: GET, HEAD`.

19. `/health` returns HTTP `200` and `{"status":"ok"}` only when SQLite is reachable and a mapping resolver is loaded. A reachable cache without a resolver returns HTTP `503` and the degraded response in invariant 16; an unreachable cache returns HTTP `503` and `{"status":"unhealthy"}`.

20. `/cache/stats` supports `GET` and `HEAD`; `/cache/clear` supports only `POST`. Unsupported methods return `405` with the corresponding `Allow` header.

21. Debug endpoints are hidden by default and return `404 Not Found`. They become available only when `DEBUG_ENDPOINTS_ENABLED=true`.

22. If `ADMIN_TOKEN` is non-empty, an enabled debug endpoint additionally requires the exact `Authorization: Bearer <token>` header. Missing or incorrect authorization returns `404`, not an authentication challenge.

23. `/cache/stats` returns JSON cache entry, hit, and miss counts. `POST /cache/clear` removes all year-cache data and returns `{"status":"ok"}`; it does not remove the mapping cache.

### Cache and upstream continuity

24. Raw, unfiltered AniList results are cached by calendar year. Filtering and TVDB resolution occur per request, so supported filter configuration changes apply after restart and refreshed mappings apply without refetching AniList data.

25. Current-year data is fresh for 24 hours; past-year data is fresh for seven days. The background scheduler checks stale data every ten minutes, evicts entries not accessed for 14 days, and checks mapping freshness every 24 hours.

26. Mapping refresh is conditional when upstream metadata permits it. A failed refresh retains the last usable local mapping. A missing or unusable mapping causes degraded behavior until a later background load succeeds.

27. TVDB resolution tries a show's MAL ID first and falls back to its AniList ID. A mapping replacement takes effect for new lookups without interrupting in-flight requests.

28. AniList requests are paginated and rate-limited. Transient rate limits are retried with `Retry-After` support and bounded exponential backoff; exhausted attempts follow the cache-miss or stale-data behavior above.

### Configuration and lifecycle

29. Configuration comes from environment variables with these defaults:

| Variable | Default | Contract |
|---|---|---|
| `PORT` | `8080` | HTTP listener port |
| `PREWARM_YEARS` | current year | Comma-separated startup prewarm years |
| `INCLUDE_TYPES` | `TV,ONA` | Comma-separated AniList formats |
| `EXCLUDE_TAGS` | empty | Comma-separated tag names |
| `FILTER_FUTURE_ENABLED` | `true` | Enables the three-month future filter |
| `MAPPING_PATH` | `/data/anibridge_mappings.json.zst` | Persistent compressed mapping path |
| `MAPPING_URL` | official anibridge v3 release URL | Mapping source |
| `CACHE_DB_PATH` | `/data/cache.db` | SQLite path |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` logging |
| `DEBUG_ENDPOINTS_ENABLED` | `false` | Exposes cache diagnostic endpoints |
| `ADMIN_TOKEN` | empty | Optional debug endpoint bearer token |

30. Invalid integers and booleans fall back to their defaults. Invalid prewarm entries are skipped; if none remain, the current year is used. List values are comma-separated, trimmed, and normalized to uppercase. Unknown `INCLUDE_TYPES` values match no shows rather than stopping startup.

31. `CACHE_DB_PATH` may be `:memory:`. Otherwise, cache and mapping paths must be absolute paths beneath `/data` or the system temporary directory; URI-like, relative, or out-of-root values fall back to defaults. `MAPPING_URL` must be HTTPS on an allowlisted anibridge/GitHub release host or it falls back to the official URL.

32. Before opening SQLite or downloading mappings, startup requires each configured cache and mapping parent directory to exist, be a directory, and be readable and writable by the runtime user. Failure stops startup.

33. The HTTP listener starts before asynchronous prewarming completes. Each configured year is fetched unless its cache entry is already fresh. A prewarm failure is logged and does not stop an already listening server.

34. `SIGINT` and `SIGTERM` cancel prewarm and background work, allow up to ten seconds for HTTP shutdown, then up to five seconds for scheduler goroutines.

### Container and release contract

35. The published image runs `/server` directly as a non-root user in a distroless image. It contains no shell, package manager, entrypoint ownership repair, or application self-updater.

36. `/data` is the expected persistent writable container path for SQLite, its sidecars, mapping data, and mapping metadata. Operators must make bind mounts readable and writable by the configured runtime UID/GID before startup. `PUID` and `PGID` are Compose substitution values for `user:`, not application environment variables.

37. The image provides a Docker healthcheck through `/server --healthcheck`, which succeeds when the local `/health` endpoint returns HTTP `200`.

38. Releases support Linux `amd64` and `arm64`. Exact version tags and digests identify immutable releases; moving `latest`, major, and minor tags select update tracks. Updates and rollbacks are external deployment operations, never in-container mutation.

<!-- markdownlint-enable MD029 -->
