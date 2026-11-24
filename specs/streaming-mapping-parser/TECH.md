# Tech Spec: Streamline Mapping Parsing

Stream zstd output into the JSON parser. Skip unused fields without retaining their encoded representation and extract only the target summaries needed for resolution. Publish after successful parsing, not incrementally.

[Product contract](PRODUCT.md). Original evidence: [PR #142](https://github.com/calmcacil/sonarr-anime-bridge/pull/142).
