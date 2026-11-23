# Country of origin filter implementation plan

**Goal:** Implement the opt-in in [PRODUCT.md](PRODUCT.md) without changing unrestricted discovery.

**Architecture:** Keep the year cache policy-neutral and apply the country allowlist to the merged show stream before TVDB resolution. Refresh legacy payloads lazily when the enabled policy needs missing metadata.

**Tech stack:** Go, AniList GraphQL, SQLite, existing `/list` pipeline.

## Context

- [AniList year query and shared fragment](https://github.com/calmcacil/sonarr-anime-bridge/blob/fa73583f387c7aa43452a85eaca215fac773ab10/internal/anilist/anilist.go#L124-L153) fetch all anime for a year, using one fragment for all page aliases.
- [List handler](https://github.com/calmcacil/sonarr-anime-bridge/blob/fa73583f387c7aa43452a85eaca215fac773ab10/cmd/server/main.go#L317-L388) waits for a missing current-year payload and schedules missing prior-year winter data in the background.
- [Scheduler](https://github.com/calmcacil/sonarr-anime-bridge/blob/fa73583f387c7aa43452a85eaca215fac773ab10/internal/scheduler/scheduler.go#L530-L577) skips usable fresh data and owns fetching and persisting the policy-neutral show array.

## Proposed changes

1. In `internal/config/config.go`, parse `DEFAULT_COUNTRIES` once. Share the country-list parser with `cmd/server/main.go`: normalize case and whitespace, deduplicate two-letter ASCII codes, and reject malformed or empty list elements. Keep blank-default and URL-only `ALL` handling at their respective boundaries. In `/list`, distinguish an absent `country` key from a present blank value, reject repeated values or malformed query encoding, and resolve the effective allowlist once per request. Reject invalid overrides with HTTP 400 before cache reads. Invalid defaults fail server startup before opening the cache; the port-only healthcheck remains independent. Log the default at startup.
2. In `internal/anilist/anilist.go`, request `countryOfOrigin` in `YearMedia` for both enabled and disabled instances. Represent origin as an optional string on `Show`. Before marshaling a successful fetch in `internal/scheduler/scheduler.go`, turn nil origins into an explicit empty value using one shared immutable value. In persisted JSON, absent/null means legacy metadata, empty means fetched but unknown, and nonempty means reported origin. Keep the existing JSON array and SQLite schema; old binaries can still read newly written rows.
3. In `internal/cache/decoded_year.go`, derive origin completeness once at decode/admission (empty arrays qualify), including both decoded-cache hits and bypasses. In `/list`, only a request with an effective allowlist requires complete metadata before serving a row. For those requests, treat an incomplete primary row as a fetch miss, recheck after fetching, and omit incompatible prior-year data while winter backfill runs. Make the scheduler fresh-row guard skip only complete payloads for any fetch already invoked, regardless of its trigger: a filtered request can then safely join a concurrent prewarm or refresh for the same year. An unrestricted request does not trigger a fetch solely because metadata is missing. Preserve existing coalescing, timeouts, cooldown, and health readiness; log `missing_country_metadata` distinctly from malformed JSON. Do not clear the database.
4. Pass the effective allowlist explicitly into processing; do not read the startup default from the scheduler or mutate shared settings for an override. Compact the caller-owned candidates in `internal/scheduler/scheduler.go` after the winter merge and before TVDB resolution. Skip the pass for unrestricted requests; otherwise exclude unknown and nonmatching values and log the dropped count. Do not filter the AniList query or cache by country: requests with different overrides reuse the same fetched year.
5. Document `DEFAULT_COUNTRIES`, `country` override precedence, `country=ALL`, and upgrade behavior in the README and baseline service docs. Add the default to deployment examples that enumerate filter settings. Do not introduce `INCLUDE_COUNTRIES`; it was only a draft name.

## Runtime verification

Build with `mise exec go -- go build -o "$work/server" ./cmd/server` and launch with disposable SQLite and mapping paths. Use the actual `/list` endpoint and retain bodies and logs. Seed current-year JP TV, JP ONA, CN, KR, and unknown-origin records with distinct TVDB IDs. Replay legacy refills through a disposable loopback HTTPS proxy, trusting its certificate only in the child process; do not add a production endpoint override for verification.

- Invariants 1-4: compare exact IDs for no default/no parameter, then on one instance with `DEFAULT_COUNTRIES=JP` prove the default, `country=CN,KR`, and `country=ALL` using the same cached year. Check normalization on `country=%20cn,KR,CN%20` and HTTP 400 for blank, repeated, or malformed values. Verify that `DEFAULT_COUNTRIES=JP,` fails startup and names the setting.
- Invariant 5: exercise `season=ALL`, `series-new`, and winter with mapped JP and CN prior-December records. With no default, verify that an explicit filtered override refills a legacy row, then reuses the enriched cache. For failed refill, verify that a filtered URL does not serve the legacy data while an unrestricted URL still can. Verify that legacy prior December is omitted until background refresh and then follows the effective allowlist.
- Invariant 6: restart the same enriched database with no default and show that unqualified URLs become unrestricted while explicit overrides are unchanged. No Go test suite or permanent proof harness is required.

## Parallelization

Use one implementation owner: the `Show` encoding and cache compatibility rule are shared by fetch and request handling, so splitting these small edits would add a coordination point. Keep the feature implementation separate from the existing Go test reduction.

## Observed AniList country codes

Live [AniList GraphQL](https://graphql.anilist.co) snapshot on 2026-11-23: paginated `Page(page: $page, perPage: 50) { pageInfo { hasNextPage } media(type: ANIME, seasonYear: 2026, sort: POPULARITY_DESC) { id countryOfOrigin } }` through page 12 (`hasNextPage: false`). The 563 distinct IDs are upstream candidates, before bridge filtering or TVDB resolution.

| Code | Origin | Records | Example AniList ID |
| --- | --- | ---: | ---: |
| `JP` | Japan | 461 | [103303](https://anilist.co/anime/103303) |
| `CN` | China | 90 | [137662](https://anilist.co/anime/137662) |
| `KR` | South Korea | 12 | [154122](https://anilist.co/anime/154122) |

Support all three observed codes: `DEFAULT_COUNTRIES=JP` gives unqualified lists Japan-only results, while `country=JP,CN,KR` selects all three on a particular URL. This snapshot is not a code registry; accept any syntactically valid two-letter code rather than hardcoding these three. No null or blank origins appeared in this snapshot; retain the specified unknown-origin behavior and verify it with an explicit unknown.
