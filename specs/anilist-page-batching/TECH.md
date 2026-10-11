# Tech Spec: Batch AniList Year Fetches

Status: implemented and validated in [PR #144](https://github.com/calmcacil/sonarr-anime-bridge/pull/144),
including required hosted CI. Manual release review remains pending.

## Context

This implements [PRODUCT.md](PRODUCT.md). Research baseline:
`89bfe4eae662e4564162760076f9cab067d2bb0f`.

- [`internal/anilist/anilist.go:128`](https://github.com/calmcacil/sonarr-anime-bridge/blob/89bfe4eae662e4564162760076f9cab067d2bb0f/internal/anilist/anilist.go#L128)
  defines the full-year/all-format query and its selected metadata fields.
- [`internal/anilist/anilist.go:250`](https://github.com/calmcacil/sonarr-anime-bridge/blob/89bfe4eae662e4564162760076f9cab067d2bb0f/internal/anilist/anilist.go#L250)
  fetches one 50-record page per POST, validates pagination, and stops at 100 pages.
- [`internal/anilist/anilist.go:236`](https://github.com/calmcacil/sonarr-anime-bridge/blob/89bfe4eae662e4564162760076f9cab067d2bb0f/internal/anilist/anilist.go#L236)
  paces a shared client at 700 ms; request handling permits five attempts with
  jittered backoff and bounded Retry-After waits. This rate exceeds the current
  advertised 30 requests/minute.
- [`internal/scheduler/scheduler.go:473`](https://github.com/calmcacil/sonarr-anime-bridge/blob/89bfe4eae662e4564162760076f9cab067d2bb0f/internal/scheduler/scheduler.go#L473)
  coordinates fetches per year and persists only a completed result. Cached hits
  and background work already share this client through the scheduler.
- [`cmd/server/main.go:337`](https://github.com/calmcacil/sonarr-anime-bridge/blob/89bfe4eae662e4564162760076f9cab067d2bb0f/cmd/server/main.go#L337)
  bounds primary misses to 90 seconds; stale responses and Winter backfill use
  asynchronous refresh. Scheduler fetches have a two-minute outer bound, and the
  HTTP client has a 30-second request timeout.
- [`cmd/server/benchmark_test.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/89bfe4eae662e4564162760076f9cab067d2bb0f/cmd/server/benchmark_test.go)
  already benchmarks hydrated 600-show cache hits, Winter hits, and parallel hits.

The live investigation retrieved the complete 2025 year twice: 482 shows in ten
single-page requests versus three four-page requests, with exact decoded field
and array-order equality. Each batch consumed one rate-limit allowance. This is
API feasibility evidence, not an application implementation or latency benchmark.
See [EVIDENCE.md](EVIDENCE.md) for the retained comparison and API constraints.

## Implemented Changes

### Query and ordered assembly

Keep `FetchYear(ctx, year) ([]Show, error)` and `Show` unchanged. Implement batching
inside `internal/anilist`; scheduler, cache, filters, and resolver contracts stay
intact. No new dependency, environment variable, or schema migration is needed.

Use a named default batch size of four, `perPage=50`, and one shared `Media`
fragment containing exactly the current selected fields. Send a single GraphQL
operation with distinct root aliases, each owning one `Page` and one `media`
field. Bind the year and page numbers as variables; keep `type: ANIME`,
`seasonYear`, and `sort: POPULARITY_DESC`. Do not add a sort tie-breaker or fetch
server-filtered subsets in this step.

Decode aliases to typed page values. Factor the existing anonymous page response
into a private reusable type; keep pointer metadata so missing/null fields can be
distinguished from valid false/empty values. Process aliases by requested page
number, never Go map iteration or JSON property order. Use static aliases or a
bounded builder; avoid rebuilding large query strings for every page attempt.

For each response:

1. Reject GraphQL errors even if partial data is present. Validate all requested
   aliases, pageInfo objects, non-null `hasNextPage`, exact `currentPage`, and
   non-null media arrays before committing that batch to the accumulated result.
2. Append pages in order through the first `hasNextPage=false`. Later aliases
   in that final batch must be valid terminal empty pages; reject inconsistent
   continuation instead of silently dropping unexpected data.
3. If every requested page has a successor, request the next consecutive batch.
   Do not use `total`, `lastPage`, or record count to infer completion.
4. Never request a page above 100. Success at page 100 requires a terminal flag;
   otherwise return the existing pagination-limit error without a partial year.
5. Preserve duplicate IDs and nested-array order. Any failed later batch returns
   an error and no accumulated partial dataset to the scheduler.

Retries resend only the failed batch; append its contents once after successful
validation. Keep the existing 10 MiB success-body and 64 KiB error-body limits,
and enforce the success-body bound rather than accepting a valid prefix of an
oversized body. Keep request timeouts, total deadlines, and retry-attempt bounds.
Do not introduce automatic fallback to single-page fetching on query errors;
such fallback could conceal incompatibility and erase request-count savings.

Memory stays proportional to the final year plus one decoded batch. Avoid a
second retained copy of the entire result or preallocating 5,000 entries from
unreliable upstream totals. The final stored representation stays compatible.

### Shared pacing and cooldown

Start with a 2.3-second request-admission interval and burst capacity one. This
permits at most 27 admissions in a rolling minute at the default rate. All
attempts use the same client gate. A larger advertised limit does not raise this
rate; for a positive lower limit, use the larger of 2.3 seconds and
`60 seconds / (0.9 * advertised limit)` to retain headroom.

Track a client-wide cooldown deadline. On 429, derive it from valid positive
Retry-After seconds and/or a future `X-RateLimit-Reset` Unix timestamp; use the
later valid deadline. With neither usable value, use the documented one-minute
timeout. A successful response with `X-RateLimit-Remaining: 0` also delays later
admission until a valid reset, or one minute when no reset is usable.
Invalid/missing headers must not remove an existing cooldown. If a server-directed
wait exceeds the existing two-minute bound,
terminate the attempt rather than retry before the server's deadline.

Combine pacing, cooldown, and exponential-backoff deadlines so waits overlap
where possible instead of adding redundant sleeps. A queued caller must recheck
the latest cooldown immediately before admission, including after a limiter
reservation: another caller can have received 429 while it waited. Ensure actual
admissions remain spaced after long waits; queued reservations must not become
a burst when a cooldown ends. Protect state with short critical sections; do not
hold a mutex across network I/O or a context-aware wait. Cancellation abandons
only that waiter and preserves the shared budget/deadline.

Use narrow clock/wait seams for deterministic admission tests. Keep existing
network/5xx retries, terminal other-4xx behavior, jitter, and safe retry logging.
Do not extend fetch deadlines to compensate for pacing. Independent processes
still require deployment-level consideration of shared-IP allowance; no
distributed rate limiter is introduced.

### Integration and diagnostics

Keep per-page debug progress after validation. Add bounded batch start/end,
HTTP-attempt count, page count, elapsed time, and pacing/cooldown wait information
to existing fetch diagnostics. Avoid request/response bodies and warm-path work.
Keep same-year deduplication, scheduler failure cooldown, write-before-publication,
prewarm hydration, and prior-year backfill through the existing APIs.

At implementation completion, update AniList ingestion in the service-baseline
spec, `docs/flow.md`, and relevant verification guidance. These documents continue
to describe the current implementation until this step ships.

## Testing and Validation

### Correctness and failure coverage

- PRODUCT 1-3: differential tests feed identical captured fields to a single-page
  reference and batched client. Compare complete ordered `[]Show`, preserving
  duplicates, null metadata, tags and relations. Exercise 0, 1, 50, 51, 200, 201,
  400, and 482 records; an empty year needs one request, and nonempty years need
  `ceil(ceil(records / 50) / 4)` requests absent retries.
- PRODUCT 3-4, 10: cover shuffled alias properties, each terminal position within
  a batch, final empty aliases, inconsistent post-terminal data, missing/null
  aliases and arrays, incorrect page numbers, absent metadata, malformed or
  oversized bodies, GraphQL errors with partial data, later-batch failure,
  cancellation, exactly 100 pages, and continuation beyond that boundary.
- PRODUCT 4-7, 10-11: scheduler/server tests prove failed batches or writes leave
  stale data intact, missing years remain absent on failure, valid empty years
  persist, retries do not duplicate results, and concurrent callers share one
  fetch. Retain Winter and stale-response nonblocking behavior and timeout/shutdown
  contracts. Run the existing ordered season/category pipeline matrix.
- PRODUCT 8-10: fake-time concurrent tests cover shared pacing and lower limits,
  429 with queued callers, absent/invalid/overflowing/past header values, exhausted
  success responses, overlapping cooldowns, canceled waiters, expiry without a
  burst, wait bounds, retry exhaustion, and a lower limit during queued admission.
  Assert actual dispatch times and rolling-minute request counts, not merely
  configured limiter values. Also retain a small real-concurrency race test.
- PRODUCT 5, 12-13: cached responses remain usable while a fake upstream is blocked
  or cooling down; assert zero upstream calls on warm hits and validate safe
  batch/page/retry diagnostics. No log-body leakage or duplicate success counts.

### Performance release criteria

Record comparative evidence on the same host/toolchain and identical snapshots
before marking PRODUCT 12 and 14 complete. Use two baselines: the current checkout
with 700 ms pacing, and a test-only unbatched reference with the candidate's
compliant pacing. The first establishes application impact; the second isolates
the batching benefit under the same allowance. Never exercise the unsafe baseline
rate against the live API.

1. Warm path: run the existing serial, Winter, and parallel hit benchmarks with
   `-benchmem -count=6` on both application revisions and compare with `benchstat`.
   A repeatable statistically significant increase above 5% in ns/op or B/op
   blocks release. Verify upstream calls and rate-limit waits remain zero. If
   results are inconclusive, collect additional matched samples; do not claim
   speed from an inconclusive comparison.
2. Fetch CPU/allocations: add a deterministic transport benchmark for decoding and
   assembling empty, small, 482-show, and 600-show years, plus a large valid year.
   Exclude fixture generation and network sleep. Report ns/op, B/op, allocs/op,
   response bytes, and HTTP attempts. For representative 482/600-show years,
   investigate and resolve repeatable increases above 10% in allocated bytes
   or CPU time. Report fixed small-year overhead rather than hiding it in averages.
3. Cold requests/prewarm: use a loopback or controlled transport with actual pacing
   and fixed 50 ms and 500 ms request latency. Include one-page, 4-, 5-, 10-, and
   12-page years, sequential two-year prewarm, and two simultaneous distinct-year
   misses. Require complete data and request counts; measure time to a complete
   cached year and `/list` response, not time to the first batch. Use at least six
   paired samples per scenario, and retain median, p95, and per-run wait breakdown.
   Candidate median time must stay within 5% of the compliant unbatched reference
   in every scenario. Ten-/12-page fetches and corresponding prewarm must also
   stay within 5% of the current 700 ms baseline. All successful scenarios must
   fit existing timeouts.
4. Required-rate waits must be visible: sparse/concurrent small fetches can wait
   longer than the old 700 ms client even when batching is faster. Report those
   comparisons and separate mandatory pacing from processing/network costs.
   This is not evidence of unchanged cold latency against the old client. Any
   unexpected slowdown, added warm wait, or timeout requires design correction
   before release; do not waive it just because request counts fell.
5. A virtual-clock test proves pacing-only bounds without flaky CI timings. For
   a fresh idle client and ten pages, old scheduled gaps total 6.3 seconds, while
   three batches at 2.3-second spacing total 4.6 seconds. This excludes network,
   server work, decode, contention, and retries; real measurements remain required.

Run `make check`, the deterministic pipeline matrix, and native release regression
with a saved snapshot. Native replay seeds caches, so it verifies list behavior
and warm performance, not the new upstream batching protocol; the client tests
must supply that coverage. Optional live confirmation stays below 30 requests
per minute and must not deliberately induce 429s. Timing thresholds are reviewed
release criteria, not machine-dependent CI assertions.

Commit baseline/candidate benchmark outputs and comparisons under this spec's
`benchmarks/` directory during implementation. Record host/toolchain, source
revisions, snapshot hashes, sample counts, request counts, failures, waits, and
the conclusion in an implementation-evidence section. Mark the TODO item complete
only after evidence passes and implementation/CI are recorded; then the existing
manual release review can proceed.

## Risks and Mitigations

Aliased queries can increase upstream work per operation and response size. The
live four-page test succeeded, but broad fixtures, error tests, and cold-latency
measurements remain required. Keep batch size bounded and existing body/time limits.
Popularity changes between requests remain an upstream consistency limitation;
use identical captures for deterministic comparisons and preserve existing order.
No larger snapshot-consistency guarantee is claimed.

## Implementation Evidence

The implementation retains `FetchYear`, Show fields, cache layout and existing
deadlines. Four static root aliases share one Media fragment. All aliases are
validated before ordered assembly, including empty terminal tails. No query
fallback or partial year publication is allowed. A mutex-owned admission gate
rechecks current pacing, cooldown and retry deadlines after every wait; it assigns
no future reservations and holds no mutex during waits or HTTP I/O. Lower
advertised limits restrict pacing, and a recovered allowance can restore the
2.3-second ceiling. Oversized waits fail without shortening shared deadlines.
Success-body decoding consumes the complete bounded body, rejecting trailing
data and oversized valid prefixes. Fetch errors and logs omit upstream bodies
and configured URLs.

Correctness tests compare all ordered fields against the retained 482-record
single-page capture and exercise 0/1/50/51/100/150/200/201/400/482/5000 records.
They validate shuffled aliases, each terminal position, incomplete/errored
batches, later failures, retry assembly, body bounds and the 100-page boundary.
Fake-clock tests establish rolling-minute limits, lower allowances, shared header
cooldowns, queued rechecks, cancellation and paced concurrent recovery; a real
concurrency test runs under the race detector. Server integration covers empty
year persistence, failed missing/stale refreshes, write failure after complete
fetch, same-year coalescing, and immediate warm Summer/Winter results during a
shared cooldown. Existing timeout, shutdown and ordered pipeline tests remain.

[Retained benchmarks and reproduction](benchmarks/README.md) record Go 1.27.2,
Linux arm64, four ARM Neoverse-N1 vCPUs, the source revision, snapshot hashes,
six samples per benchmark/scenario, raw dispatch/completion times, output hashes,
request counts and derived network/pacing/processing breakdowns. All 30 controlled
scenarios pass across the original, compliant-reference and candidate variants
(540 samples total).

| Release criterion | Observed result |
| --- | --- |
| Warm serial/Winter/parallel responsiveness and bytes | No >5% regression; zero upstream calls on cached hits during cooldown. Lower measured ns/op are not attributed to batching. |
| Representative fetch CPU/bytes | 482 records: CPU -4.44%, bytes +1.48%, attempts 10 -> 3. 600 records: CPU -2.44%, bytes -4.18%, attempts 12 -> 3. |
| Fixed small-year cost | Empty/one-record decode costs about 8-9 us and 1.5-2.8 KiB extra; retained separately. |
| Cold/prewarm/concurrent vs compliant reference | Every median within 5%; worst positive difference +0.15%. Complete data and list digests match. |
| Ten-/twelve-page vs original client | All cold, concurrent and prewarm medians improve at both latencies. Ten-page cold: 6.38 -> 4.70 s at 50 ms; 6.83 -> 5.13 s at 500 ms. |
| Required pacing cost | Two one-page years at 50 ms: 0.76 -> 2.36 s; compliant reference 2.36 s. Five-page two-year operations also show the stricter budget cost against the old client. |
| Existing timeouts | Candidate completes every controlled run in about 12.1 s or less; no budgets increased. |
| Local verification | `make check`, race-tested pipeline matrix, shared-gate race checks and checksummed native `v2.15.2` replay pass. |
| Hosted CI | [PR #144](https://github.com/calmcacil/sonarr-anime-bridge/pull/144) passes Required, PR title, all CodeQL languages, quality, race tests, vulnerabilities, dependency review and native amd64/arm64 container smoke/image scans. [Immutable implementation-run evidence](benchmarks/ci-validation.json). |
| Manual release | Full-batch review remains pending; Release Please PR #140 stays open with auto-merge disabled. |

The native replay checks ordered list behavior, endpoints, restart persistence,
clear and shutdown; its saved cache inputs do not test the new upstream protocol.
Client and server integration tests provide that protocol coverage. No live
requests or deliberately induced upstream rate limits were needed for validation.

## Parallelization

Use one implementation agent/branch. Query assembly, retry behavior, shared
admission, and their tests are tightly coupled in the AniList client. Complete
client correctness first, then integration and comparative performance evidence,
and finally the full local/CI checks in one focused PR. Parallel agents add little
benefit for this scope and are not proposed.
