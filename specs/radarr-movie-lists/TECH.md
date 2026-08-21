# Tech Spec: Radarr Anime Movie Lists

## Context

This plan implements [PRODUCT.md](./PRODUCT.md) against commit [`a0fc26a65503a62bd087d55103d437fbeab02f94`](https://github.com/calmcacil/sonarr-anime-bridge/tree/a0fc26a65503a62bd087d55103d437fbeab02f94).

- [`internal/mapping/anibridge.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/internal/mapping/anibridge.go) streams each top-level `mal:*` or `anilist:*` entry, decodes its descriptor map, and currently retains only the preferred `tvdb_show` target. `AnibridgeMapping` has separate MAL and AniList TVDB maps.
- [`internal/mapping/resolve.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/internal/mapping/resolve.go) owns atomic mapping swaps and MAL-first/AniList-fallback TVDB resolution. A single immutable mapping object must carry both series and movie indexes so refresh remains one atomic operation.
- [`internal/scheduler/scheduler.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/internal/scheduler/scheduler.go) decodes cached AniList rows, merges winter overflow, filters, resolves, logs aggregate counts, and converts results to the Sonarr response. Series processing also writes `seen_mappings`; movie processing must not.
- [`internal/filter/filter.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/internal/filter/filter.go) combines the short-duration and excluded-tag filters in `FilterWithStats`. PRODUCT 15 requires movie processing to apply tags without applying duration.
- [`cmd/server/main.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/cmd/server/main.go) owns routing, query validation, resolver degradation, cache-miss fetch, winter backfill, stale refresh, and response writing for `/list`. The new endpoint must share these state transitions without changing the Sonarr contract.
- [`internal/anilist/anilist.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/internal/anilist/anilist.go) already fetches `MOVIE`, supplies MAL/AniList identifiers, and defines the required English/romaji/generated display-title fallback.

No SQLite schema or AniList query change is required. The raw year cache already contains movie records, and the scheduler's in-flight fetch map already coalesces work by year.

## Proposed changes

### Mapping indexes and parser

1. Expand `AnibridgeMapping` to hold four immutable indexes:

   ```go
   byMALTVDB      map[int]int
   byAniListTVDB  map[int]int
   byMALIMDBMovie map[int]string
   byAniListIMDBMovie map[int]string
   ```

   Rename the existing generic fields so their provider is explicit. Update the constructor and every internal caller in one cutover; do not retain aliases for the old field names.

2. Keep existing `LookupByMAL` and `LookupByAniList` TVDB methods and their behavior. Add provider-specific movie methods `LookupMovieIMDBByMAL(int) (string, bool)` and `LookupMovieIMDBByAniList(int) (string, bool)`.

3. Refactor the parser so each source entry's target object is decoded once into `map[string]json.RawMessage`. Run TVDB selection and IMDb movie extraction over that decoded map. The current decoder-consuming `extractTVDB` cannot be called independently for two providers without this refactor.

4. IMDb extraction accepts only descriptors of the exact form `imdb_movie:tt<digits>`, with at least one digit and no additional descriptor segment. Preserve the complete `tt...` value in the index. Ignore malformed provider descriptors without failing the entire mapping load; malformed JSON structure remains a parse error.

5. For the same source key, descriptor-map iteration must not make selection nondeterministic. If multiple distinct valid `imdb_movie` descriptors exist, sort candidate IDs and select the lexically first value while logging one aggregate/debug ambiguity signal rather than depending on Go map order.

6. Preserve existing TVDB parsing, TVDB lookup results, resolver refresh fallback, and atomic swap behavior. Existing mapping `Stats`, metadata keys, and update-count semantics should remain series-compatible; add explicit movie counts to parser/load logs rather than silently changing the meaning of existing series counts.

### Movie resolver

7. Add a movie result type in `internal/mapping/resolve.go`:

   ```go
   type ResolvedMovie struct {
       IMDBID   string
       Title    string
       Resolved bool
   }
   ```

8. Add `ResolveMovieIMDB(anilist.Show) (string, bool)` and `ResolveMovieBatch([]anilist.Show) map[int]ResolvedMovie`. Mirror existing resolution ownership: load the active mapping once per call, try a positive MAL ID first, then a positive AniList ID, and use `DisplayTitle()` for the result title. A nil resolver mapping or invalid/unmapped IDs returns a clean miss.

9. Keep movie lookup diagnostics at debug level and aggregate request results in the scheduler. Do not emit a warning for each unmapped movie.

### Filtering and scheduler processing

10. Extract the winter merge into a scheduler helper shared by series and movies. It must preserve current behavior: only `WINTER` reads prior-year cached data, only December starts are appended, AniList IDs are deduplicated, corrupt prior-year JSON is logged and ignored, and `ALL` never merges prior-year data.

11. Separate excluded-tag filtering from duration filtering in `internal/filter/filter.go`. Keep `FilterWithStats` behavior unchanged for series. Add a tag-only function with stats for movie processing rather than adding a default-sensitive flag that could change existing callers.

12. Add the public response type:

   ```go
   type Movie struct {
       Title  string `json:"title"`
       IMDBID string `json:"imdb_id"`
   }
   ```

13. Add `ProcessMoviesContext(ctx, rawData, season, year) ([]Movie, error)` with this ordered pipeline:
   - Decode the cached AniList year.
   - Merge prior-year winter overflow when requested.
   - Apply season filtering.
   - Hard-filter to `MOVIE`.
   - Apply excluded tags without `SkipByDuration`.
   - Apply the existing three-month future filter only when enabled.
   - Resolve IMDb movie mappings in one batch.
   - Walk the filtered AniList slice in order, omit unresolved entries, and deduplicate by IMDb ID while keeping the first.

14. Emit one aggregate movie-processing log with `type=filter`, a movie-specific message or field, `year`, `season`, input/after-filter counts, resolved, unresolved, duplicate count, and `duration_ms`. Do not call `trackNewMappings` or write `seen_mappings`.

15. Add non-context convenience methods only if existing scheduler conventions require them; the HTTP path must use the request context.

### HTTP endpoint and shared request lifecycle

16. Register `/movies/list` unconditionally beside `/list` and pass the existing cache, scheduler, and config dependencies.

17. Extract small shared helpers for season/year parsing so `/list` and `/movies/list` cannot drift. Preserve exact existing validation messages and year-window calculation. Keep category parsing in `/list` only.

18. Implement `handleMovieList` with the same ordering as `handleList`:
   - Reject unsupported methods.
   - Parse season and year.
   - Return the existing resolver-unloaded `503` JSON.
   - Read the requested year.
   - On miss, synchronously call `FetchAndStore(..., "cache_miss")`, reread, and return `[]` if the fetch fails or data remains absent.
   - For winter, schedule missing prior-year fetch with trigger `winter_overflow`.
   - Process through `ProcessMoviesContext`.
   - Schedule requested-year refresh with trigger `stale_refresh` after serving stale data.
   - Marshal a non-nil movie slice and write JSON.

19. Share cache-lifecycle helpers only where they keep logging and error behavior explicit. Do not introduce a generic endpoint framework or change `/list` ordering as incidental cleanup. The existing in-flight `FetchAndStore` behavior supplies PRODUCT 24 without endpoint-specific synchronization.

20. Ensure empty results allocate or normalize to `[]Movie{}` before marshaling so JSON is `[]`, not `null`. Actual HTTP `HEAD` behavior must suppress the body while retaining GET-equivalent status and headers.

### Documentation

21. Update README usage, endpoint behavior, architecture flow, and allowed-method documentation. Add a concise Radarr setup section selecting **StevenLu Custom**, showing the `/movies/list?year=YYYY` URL, and explaining IMDb mapping coverage and the absence of release-availability filtering.

22. Update `docs/flow.md` and the service baseline specs where the shipped endpoint inventory or mapping model would otherwise become stale. Do not document TMDb mode, release-region settings, or an enable flag.

## Testing and validation

- **Mapping parser (PRODUCT 25-26, 28, 30):** fixtures for MAL and AniList `imdb_movie` targets, entries carrying both TVDB and IMDb descriptors, malformed IMDb descriptors, multiple candidates with deterministic selection, movie-only entries, and unchanged TVDB season-scope preference.
- **Resolver (PRODUCT 6-8, 25-29):** MAL preference, AniList fallback, malformed/missing IDs, unloaded resolver, batch title fallback, and atomic replacement exposing new movie mappings while retaining the previous mapping after failed refresh.
- **Filters/scheduler (PRODUCT 7, 12-18, 29):** MOVIE-only selection independent of `INCLUDE_TYPES`, tag exclusion, future-filter enabled/disabled, inclusion of ≤10-minute movies, winter merge, no merge for `ALL`, unresolved omission, stable IMDb deduplication, empty arrays, and unchanged `seen_mappings` count.
- **HTTP (PRODUCT 1-5, 9-11, 19-24, 27):** route registration, GET/HEAD/405, season/year defaults and invalid inputs, resolver `503`, cached response shape, cache-miss success/failure, absent-after-fetch, stale response plus refresh trigger, winter backfill trigger, cache/process errors, and cross-endpoint in-flight fetch coalescing.
- **Regression (PRODUCT 30):** retain all existing mapping, scheduler, `/list`, health, debug, and cache tests; update constructor call sites without weakening assertions.
- **Documentation (PRODUCT 31-32):** run `python3 scripts/check-doc-links.py` and review Radarr setup against the exact StevenLu Custom payload.
- Run `go test -race ./...`, `go vet ./...`, `golangci-lint run ./...`, `go build ./...`, and the repository `make check` gate.
- Perform a live Radarr smoke when available: configure StevenLu Custom with a controlled `/movies/list` response and confirm Radarr's Test action accepts the URL and imports an IMDb-mapped movie. This validates external compatibility but does not block deterministic repository tests when Radarr is unavailable.

## Risks and mitigations

- **Radarr format confusion:** support only source-verified StevenLu Custom and name it explicitly in docs.
- **Parser regression:** decode target maps once, preserve TVDB selection tests, and atomically swap one mapping containing both provider families.
- **Duplicate imports:** deduplicate by IMDb ID after resolution while retaining source order.
- **Short movies silently disappear:** use a tag-only filter and test the ten-minute boundary.
- **Endpoint drift:** share narrow season/year helpers and mirror the established cache lifecycle with contract tests.
- **Mapping coverage is incomplete:** omit unresolved records, expose aggregate counts, and never emit invalid list entries.
- **Availability is mistaken for release readiness:** state explicitly that no TMDb release-date decision is made.

## Parallelization

Implementation can use three local agents with a fixed mapping/scheduler contract:

- **Mapping agent** — worktree `/home/calmcacil/worktree/feat-39-radarr-mapping`, branch `feat/39-radarr-mapping`; owns `internal/mapping/` and defines `ResolvedMovie`, the four mapping indexes, and movie resolver methods.
- **Pipeline agent** — worktree `/home/calmcacil/worktree/feat-39-radarr-pipeline`, branch `feat/39-radarr-pipeline`; owns `internal/filter/`, `internal/scheduler/`, `cmd/server/`, and their tests. It consumes the mapping API fixed above.
- **Documentation agent** — worktree `/home/calmcacil/worktree/feat-39-radarr-docs`, branch `docs/39-radarr-movie-lists`; owns README, `docs/flow.md`, and baseline-spec updates.

The mapping and documentation agents can start immediately. The pipeline agent can scaffold filters and HTTP lifecycle concurrently, but final resolver integration depends on the mapping API. Merge all work into `feat/39-add-radarr-movie-lists` and use one PR for issue #39; run final repository and live Radarr validation only after integration.
