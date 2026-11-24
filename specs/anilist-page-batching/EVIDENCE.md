# AniList batching evidence

On 2026-10-11, the single-page and four-page queries returned the same ordered 2025 records, including nested fields. This is a historical observation, not a current rate-limit guarantee.

| Measure | Single-page reference | Four-page candidate |
| --- | --- | --- |
| Complete records | 482 | 482 |
| Pages containing records | 10 | 10 |
| HTTP requests | 10 | 3 |
| Last nonempty page | Page 10: 32 records | Page 10: 32 records |
| Exact decoded data and order | Reference | Equal to reference |


[Controlled measurements](benchmarks/README.md).
