# Tech Spec: Batch AniList Year Fetches

The year query groups four aliased pages per POST and assembles them in numeric order through the terminal page. All callers share pacing and rate-limit cooldown. See the [original comparison](EVIDENCE.md) and [retained measurements](benchmarks/README.md).

[Product contract](PRODUCT.md). Original evidence: [PR #144](https://github.com/calmcacil/sonarr-anime-bridge/pull/144).
