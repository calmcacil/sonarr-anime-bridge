# Country of origin filter implementation

Persist origin with every newly fetched show. Missing metadata identifies legacy arrays; a present empty value means the origin is unknown. Refresh incompatible primary rows before serving a restricted list; backfill incompatible prior-year winter rows asynchronously. The missing-metadata trigger can re-enter a coalesced fetch after a normal fresh-row skip. Apply the effective allowlist per request, before TVDB resolution; do not partition the year cache.

[Product contract](PRODUCT.md).

## Observed AniList country codes

Live [AniList GraphQL](https://graphql.anilist.co) snapshot on 2026-11-23: paginated `Page(page: $page, perPage: 50) { pageInfo { hasNextPage } media(type: ANIME, seasonYear: 2026, sort: POPULARITY_DESC) { id countryOfOrigin } }` through page 12 (`hasNextPage: false`). The 563 distinct IDs are upstream candidates, before bridge filtering or TVDB resolution.

| Code | Origin | Records | Example AniList ID |
| --- | --- | ---: | ---: |
| `JP` | Japan | 461 | [103303](https://anilist.co/anime/103303) |
| `CN` | China | 90 | [137662](https://anilist.co/anime/137662) |
| `KR` | South Korea | 12 | [154122](https://anilist.co/anime/154122) |

These observations are not a registry: accept any two-letter ASCII code.
