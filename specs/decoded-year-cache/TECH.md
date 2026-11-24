# Tech Spec: Reuse Decoded Year Data

SQLite remains authoritative. The bounded in-memory cache reuses immutable decoded shows for matching payloads. Per-year coordination avoids redundant decoding; invalidation prevents an in-flight decode from publishing stale data. Request filtering must not mutate shared slices.

[Product contract](PRODUCT.md). Original evidence: [PR #139](https://github.com/calmcacil/sonarr-anime-bridge/pull/139).
