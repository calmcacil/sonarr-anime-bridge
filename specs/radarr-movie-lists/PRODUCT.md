# Product Spec: Radarr Anime Movie Lists

## Summary

Sonarr Anime Bridge adds an always-available `/movies/list` endpoint that lets Radarr import anime movies through its URL-configurable StevenLu Custom list. The endpoint reuses the existing AniList year cache and request behavior while returning IMDb movie identifiers instead of TVDB series identifiers.

## Goals

- Let one service instance provide Sonarr series lists and Radarr movie lists simultaneously.
- Produce the source-verified StevenLu Custom payload Radarr accepts from a user-configured URL.
- Keep movie cache, season, tag, future-date, and degraded-state behavior consistent with `/list` where those behaviors apply.

## Non-goals

- Provide TMDb output or claim that Radarr accepts a custom TMDb-only URL payload.
- Call the TMDb API or determine theatrical, digital, physical, television, regional, or other release availability.
- Include AniList `OVA`, `SPECIAL`, or any format other than `MOVIE`.
- Add an endpoint enable flag, authentication, or movie-specific filtering configuration.
- Change the existing `/list` response or series processing behavior.

## Behavior

### Endpoint and response

1. `GET /movies/list` returns an `application/json` array in Radarr's URL-configurable StevenLu Custom shape:

   ```json
   [
     {
       "title": "Movie Title",
       "imdb_id": "tt1234567"
     }
   ]
   ```

2. `HEAD /movies/list` returns the same status and headers as the equivalent `GET` request and no response body.

3. Methods other than `GET` and `HEAD` return HTTP `405` with `Allow: GET, HEAD`.

4. The endpoint is always registered and needs no enable flag. Existing debug-endpoint configuration and admin-token behavior do not apply to it.

5. The response always encodes an array. When no movies qualify, no mapping is available for any qualifying movie, or an upstream cache-miss fetch fails, the successful response body is `[]`, never `null` or an object wrapper.

6. Each result contains exactly:
   - `title`: the existing AniList display title, preferring English, then romaji, then the existing `Anime #<AniList ID>` fallback.
   - `imdb_id`: a valid mapped IMDb movie identifier, including the `tt` prefix.

7. Results retain the order of qualifying AniList records after winter overflow is merged. If multiple AniList records resolve to the same IMDb ID, only the first record in that order is returned.

8. Movies without a valid IMDb movie mapping are omitted. One unresolved or malformed mapping does not fail the request or create a partial object without `imdb_id`.

### Query parameters

9. `season` accepts `WINTER`, `SPRING`, `SUMMER`, `FALL`, and `ALL`, case-insensitively after trimming whitespace. Missing or empty `season` defaults to `ALL`. Any other value returns HTTP `400` with the existing invalid-season response.

10. `year` accepts a positive integer within ten years before or after the current year. Missing `year` defaults to the current year. Malformed, non-positive, or out-of-range values return HTTP `400` with the same validation behavior as `/list`.

11. `/movies/list` does not define `category` or output-format parameters. It always emits StevenLu Custom IMDb records; `INCLUDE_TYPES` and the `/list` `category` behavior do not alter movie results.

### Movie selection

12. Only AniList records whose format is exactly `MOVIE` are eligible, regardless of `INCLUDE_TYPES`. Adding `MOVIE` to `INCLUDE_TYPES` is not required, and removing series formats from `INCLUDE_TYPES` does not affect `/movies/list`.

13. `EXCLUDE_TAGS` applies to movies with the same case-insensitive tag matching used by `/list`. A movie with any excluded tag is omitted.

14. `FILTER_FUTURE_ENABLED` applies to movies using the existing three-month future window. When disabled, the future-date filter does not exclude movies.

15. The existing short-duration filter does not apply to `/movies/list`. An AniList `MOVIE` remains eligible when its duration is ten minutes or less or is unknown.

16. Season filtering uses the same AniList season and start-month interpretation as `/list`. With `season=ALL`, no season filter is applied.

17. `season=WINTER` can include December-starting `MOVIE` records from the prior year's cache. Duplicate AniList records introduced by the merge are included only once before IMDb deduplication.

18. Movie inclusion does not assert that a title is available outside theatres or in any particular region. Radarr and the operator remain responsible for availability decisions.

### Cache and failure behavior

19. `/movies/list` reads the same raw one-row-per-year cache as `/list`; it does not create a separate movie cache or require a database migration.

20. On a requested-year cache miss, the request synchronously fetches and stores that AniList year before processing movies. A fetch failure returns HTTP `200` with `[]`, matching `/list` cache-miss behavior.

21. When requested-year cached data is stale, the endpoint returns results from that row immediately and schedules the existing asynchronous year refresh. A refresh failure does not invalidate the response or delete stale data.

22. For `WINTER`, when the prior-year row is absent, the endpoint schedules the same asynchronous prior-year backfill used by `/list`. The current response may omit prior-December movies; later requests include them after backfill succeeds.

23. Cache read failures and invalid cached JSON return the same server-error status used by `/list`. Errors are not embedded in the Radarr array.

24. Concurrent `/list` and `/movies/list` cache misses or refreshes for the same year share the existing in-flight year fetch. Movie support must not multiply upstream AniList requests for the same year.

### Resolver and compatibility

25. Movie resolution uses anibridge `imdb_movie` descriptors. A movie with both MAL and AniList identifiers prefers the IMDb mapping associated with its MAL ID and falls back to its AniList ID when MAL is missing or unmapped.

26. Invalid IMDb descriptors are ignored. TVDB show, TVDB movie, and TMDb movie descriptors are not used as fallback identifiers for this endpoint.

27. When the resolver has not loaded any mapping, `/movies/list` returns HTTP `503` with the same degraded JSON response as `/list`.

28. Mapping refreshes become visible to movie requests through the same active resolver swap as series mappings. A refresh failure keeps the previously loaded series and movie mappings active.

29. Movie resolution emits aggregate processing counts suitable for diagnosing input, filtering, resolved, unresolved, and duplicate results. It does not record movie entries in the series-oriented `seen_mappings` store and does not log one routine warning per unresolved movie.

30. Existing `/list`, `/health`, cache/debug endpoints, series TVDB resolution, configuration defaults, and background scheduling remain unchanged except that a mapping load also retains IMDb movie descriptors for `/movies/list`.

### Radarr setup

31. Operator documentation identifies Radarr's URL-configurable **StevenLu Custom** import list as the supported integration and uses a URL such as:

   ```text
   http://sonarr-anime-bridge:8080/movies/list?year=2026
   ```

32. Documentation does not recommend Radarr's built-in StevenLu List as a custom URL target and does not document TMDb-only output until that path is separately validated and specified.
