# Country of origin filter implementation plan

**Goal:** Implement the opt-in in [PRODUCT.md](PRODUCT.md) without changing unrestricted discovery.

**Architecture:** Keep the year cache policy-neutral and apply the country allowlist to the merged show stream before TVDB resolution. Refresh legacy payloads lazily when the enabled policy needs missing metadata.

**Tech stack:** Go, AniList GraphQL, SQLite, existing `/list` pipeline.

## Context

- [AniList year query and shared fragment](https://github.com/calmcacil/sonarr-anime-bridge/blob/fa73583f387c7aa43452a85eaca215fac773ab10/internal/anilist/anilist.go#L124-L153) fetch all anime for a year, using one fragment for all page aliases.
- [List handler](https://github.com/calmcacil/sonarr-anime-bridge/blob/fa73583f387c7aa43452a85eaca215fac773ab10/cmd/server/main.go#L317-L388) waits for a missing current-year payload and schedules missing prior-year winter data in the background.
- [Scheduler](https://github.com/calmcacil/sonarr-anime-bridge/blob/fa73583f387c7aa43452a85eaca215fac773ab10/internal/scheduler/scheduler.go#L530-L577) skips usable fresh data and owns fetching and persisting the policy-neutral show array.

## Proposed changes

1. In `internal/config/config.go`, parse `INCLUDE_COUNTRIES` once into normalized, deduplicated two-letter ASCII codes. Trim the whole value to detect the disabled state; reject malformed tokens in non-blank settings. Have the full server config load return the validation error to `run`, before opening the cache or starting the server. Keep the port-only healthcheck independent of filter validation. Log the effective allowlist with the other filter settings.
2. In `internal/anilist/anilist.go`, request `countryOfOrigin` in `YearMedia` for both enabled and disabled instances. Represent origin as an optional string on `Show`. Before marshaling a successful fetch in `internal/scheduler/scheduler.go`, turn nil origins into an explicit empty value using one shared immutable value. In persisted JSON, absent/null means legacy metadata, empty means fetched but unknown, and nonempty means reported origin. Keep the existing JSON array and SQLite schema; old binaries can still read newly written rows.
3. In `internal/cache/decoded_year.go`, derive whether all shows carry origin metadata once at decode/admission (empty arrays qualify). Ensure both decoded-cache hits and bypasses provide the same result. In `cmd/server/main.go` and the scheduler fresh-row guard, use the same policy-aware compatibility predicate: ignore origin completeness when disabled; otherwise treat incompatible data as a miss for fetching purposes. Check compatibility again after synchronous fetch; do not use prior-year legacy data while its normal backfill runs. Use existing coalescing, timeouts, and cooldown. Log `missing_country_metadata` distinctly from malformed JSON. Do not clear the database or change health readiness semantics.
4. In `internal/scheduler/scheduler.go`, compact the caller-owned candidate slice after winter merge and before resolution. Skip the pass entirely when disabled; otherwise exclude unknown and nonmatching values and include the dropped count in aggregate debug logs. Do not filter the AniList query itself: sharing or reopening the database under a different allowlist must not require rebuilding already enriched years.
5. Document the variable, upgrade behavior, and example in the README configuration reference and existing deployment examples that enumerate filter settings. Update the baseline service behavior docs with the finalized pipeline behavior in the implementation change.

## Runtime verification

Build the server with `mise exec go -- go build -o "$work/server" ./cmd/server` and launch it with disposable SQLite and mapping paths. Use the actual `/list` endpoint and retain response bodies and relevant logs. For deterministic proof, seed throwaway data with uniquely mapped JP TV, JP ONA, CN, and unknown-origin shows; use fresh arrays for the default/opt-in comparison so fixture data does not trigger network access. Use an upstream replay through a disposable loopback HTTPS proxy for the legacy-refill scenario; trust its certificate only in the child process and do not introduce an application endpoint override for verification.

- Invariants 1-3: compare the exact returned TVDB IDs with the setting unset and `INCLUDE_COUNTRIES=' jp,JP '`. With `INCLUDE_COUNTRIES='JP,'`, verify startup exits nonzero with the setting named in the error.
- Invariant 4: check `ALL`, `series-new`, and `WINTER` including a mapped prior-December JP and CN record. Replay a successful legacy refill and verify the first current-year response and subsequent cache hit exclude CN; then replay a failed refill against a separate legacy database and verify the old unrestricted rows are not served. For legacy prior December, verify it is omitted until background refresh completes, then returned only for JP.
- Invariant 5: restart on the same enriched database without the setting, then show that eligible CN and unknown-origin records return with no forced refill. No Go test suite or permanent proof harness is required.

## Parallelization

Use one implementation owner: the `Show` encoding and cache compatibility rule are shared by fetch and request handling, so splitting these small edits would add a coordination point. Keep the feature implementation separate from the existing Go test reduction.
