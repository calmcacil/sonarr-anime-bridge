# Country of origin filter

Operators who use the bridge as a Japanese-anime list need to exclude other productions without losing Japanese shows distributed online. The bridge currently accepts eligible AniList anime regardless of origin.

## Behavior invariants

1. An unset or blank `INCLUDE_COUNTRIES` preserves existing list membership, including shows with unknown origin.
2. `INCLUDE_COUNTRIES=JP` admits only records AniList marks as Japanese. Accept comma-separated two-letter country codes, ignoring case and surrounding whitespace. Duplicates have no effect. A non-blank setting with a malformed or empty entry fails startup and identifies the offending setting instead of silently enabling unrestricted imports.
3. When enabled, missing or unknown origin does not pass. Use AniList's origin, not language, title, studio, streaming format, or the country of the source material. `TV` and `ONA` remain eligible under the existing format rules.
4. Apply the policy to every `/list` response, including `season=ALL`, `series-new`, and the prior December included in winter. Old cache entries must not leak nonmatching shows or require a manual cache clear. If necessary metadata cannot be fetched, preserve the existing cache-miss behavior for the requested year and omit unavailable prior-year entries.
5. Unsetting the option restores unrestricted discovery after restarting the bridge. This setting neither removes shows already in Sonarr nor affects imports from other lists, and it does not change season boundaries.
