# AniList Batching Research Evidence

Captured on 2026-10-11 against `https://graphql.anilist.co`. This records the
exploratory API validation that motivated [PRODUCT.md](PRODUCT.md) and
[TECH.md](TECH.md). Application implementation and comparative performance are
now recorded in its implementation-evidence section and [benchmarks](benchmarks/README.md).
Required hosted CI and manual release review remain pending.

## Complete-Year Comparison

The reference sent the exact `yearQueryTemplate` from research revision
`89bfe4eae662e4564162760076f9cab067d2bb0f`, with year 2025, page size 50, and
consecutive pages until `hasNextPage=false`. The candidate used identical media
arguments and selected fields in four aliased root `Page` queries per POST,
sharing a `Media` fragment. Results were assembled in numeric page order through
the terminal page.

| Measure | Single-page reference | Four-page candidate |
| --- | --- | --- |
| Complete records | 482 | 482 |
| Pages containing records | 10 | 10 |
| HTTP requests | 10 | 3 |
| Last nonempty page | Page 10: 32 records | Page 10: 32 records |
| Exact decoded data and order | Reference | Equal to reference |

The direct comparison included every requested field, optional value, tag array,
relation array, and record order. No sorting, normalization, or ID-only comparison
was used for this result. The candidate's final request also returned valid
terminal empty pages 11 and 12. No retry or 429 occurred.

Both decoded arrays, serialized with Python's
`json.dumps(data, sort_keys=True, separators=(',', ':'))`, have SHA-256
`54d995bc5e0f0330345df1b256960840e6f0c4058aca4e1acaec8f646fdfb0b1`.
Object keys are canonicalized only for this digest; array ordering is preserved.

Each batch consumed one rate-limit allowance: the response remaining count went
from 20 after the reference's last request to 19, 18, and 17 after the three
batches. Every response advertised a limit of 30 requests/minute. These are fewer
HTTP operations, not fewer underlying media-page resolutions inside AniList.

## Additional Probes and Limits

- A request with `perPage=100` returned only 50 records.
- TV/ONA filtering returned 287 records in two batched requests. Show order and
  field contents matched local filtering, with tag-order differences. This subset
  optimization is outside the proposed scope because it changes cache contents.
- The full 2025 reference contained four Winter entries starting in December
  2024. A separate 2026 sample returned only `seasonYear=2026`. The reference's
  description that `seasonYear` requires a season argument does not fully match
  the observed year-only behavior. Winter backfill remains unchanged.
- Eighteen total GraphQL requests were made, at least 2.3 seconds apart within
  each run, with a 55.5-second gap between runs. Rate-limit exhaustion was not
  intentionally tested.

The original payloads, response bodies, headers, probe script, and comparison
results were retained locally in `/tmp/anilist-api-review/`. Temporary artifacts
may expire; implementation must commit deterministic fixtures and performance
evidence. This complete-year equivalence result covers 2025, not every year or
injected error condition. The probe does not establish application latency or
throughput, even though the request count dropped by 70%.

## API Documentation

- [Rate limiting](https://docs.anilist.co/guide/rate-limiting): current degraded
  limit of 30/minute, response headers, one-minute timeout, and burst limiting.
- [Pagination](https://docs.anilist.co/guide/graphql/pagination): one data field
  per `Page`, with `pageInfo` exempt; rely on `hasNextPage`, not inaccurate totals.
- [Root query](https://docs.anilist.co/reference/query): maximum page size of 50.
- [Page](https://docs.anilist.co/reference/object/page): maximum depth of 5,000
  entries and media query arguments.
- [Media](https://docs.anilist.co/reference/object/media): returned metadata,
  including relations and update time.
