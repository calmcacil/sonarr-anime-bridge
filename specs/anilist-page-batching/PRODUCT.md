# Product Spec: Batch AniList Year Fetches

Fetch a complete year with fewer upstream requests without changing record order, dropping duplicates or publishing partial results. Cache hits do not consume the AniList budget.

[Implementation](TECH.md). Original evidence: [PR #144](https://github.com/calmcacil/sonarr-anime-bridge/pull/144).
