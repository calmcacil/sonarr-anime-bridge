# Country of origin filter

Operators who use the bridge as a Japanese-anime list need to exclude other productions without losing Japanese shows distributed online. The bridge currently accepts eligible AniList anime regardless of origin.

## Behavior invariants

1. A `/list` URL without `country` uses `DEFAULT_COUNTRIES`. When the setting is unset or blank, those URLs keep existing unrestricted behavior, including shows with unknown origin.
2. `DEFAULT_COUNTRIES=JP` makes unqualified lists Japan-only. A single `country` parameter replaces, rather than extends, that default: `/list?country=CN,KR` selects China and South Korea; `/list?country=ALL` explicitly disables the filter for that URL. Different Sonarr lists can use different URLs on the same bridge.
3. Country lists accept comma-separated two-letter codes, ignoring case and surrounding whitespace; duplicates have no effect. `ALL` is reserved for an explicit URL override and cannot be combined with codes. A malformed configured default fails startup and names `DEFAULT_COUNTRIES`; a blank, malformed, or repeated `country` parameter returns HTTP 400 rather than falling back to the default.
4. With a country allowlist, missing or unknown origin does not pass. Use AniList's origin, not language, title, studio, streaming format, or the country of the source material. `TV` and `ONA` remain eligible under existing format rules.
5. Apply the effective filter to every `/list` variant, including `season=ALL`, `series-new`, and the prior December included in winter. Legacy cache entries must not leak nonmatching shows or require a manual clear. If necessary metadata cannot be fetched, preserve existing cache-miss behavior for the requested year and omit unavailable prior-year entries.
6. Unsetting the default and restarting restores unrestricted discovery for URLs without `country`; explicit URLs keep their behavior. This is a default, not an access restriction. It does not remove shows already in Sonarr, change other import sources, or change season boundaries.
