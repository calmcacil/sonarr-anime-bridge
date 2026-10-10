# Tech Spec: Reuse Decoded Year Data

Status: proposed; implement after review of this spec and [PRODUCT.md](./PRODUCT.md).

## Context

The optimization serves [PRODUCT.md](./PRODUCT.md). Research baseline:
`3f7a54d3c8cc7238ed611cb05de5639cc022ec23`.

- [`cmd/server/main.go:318`](https://github.com/calmcacil/sonarr-anime-bridge/blob/3f7a54d3c8cc7238ed611cb05de5639cc022ec23/cmd/server/main.go#L318)
  reads a SQLite BLOB, then decodes the entire year for each request. Winter can
  repeat this for the previous year.
- [`internal/scheduler/scheduler.go:206`](https://github.com/calmcacil/sonarr-anime-bridge/blob/3f7a54d3c8cc7238ed611cb05de5639cc022ec23/internal/scheduler/scheduler.go#L206)
  validates an array and calls `json.Unmarshal`. Prewarm and fetch coordination
  also decode persisted data to validate it.
- [`internal/scheduler/scheduler.go:284`](https://github.com/calmcacil/sonarr-anime-bridge/blob/3f7a54d3c8cc7238ed611cb05de5639cc022ec23/internal/scheduler/scheduler.go#L284)
  and [`internal/filter/filter.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/3f7a54d3c8cc7238ed611cb05de5639cc022ec23/internal/filter/filter.go)
  consume caller-owned slices, compacting them in place. Nested tags, relations,
  titles, and optional-number pointers are currently only read.
- [`internal/cache/cache.go:313`](https://github.com/calmcacil/sonarr-anime-bridge/blob/3f7a54d3c8cc7238ed611cb05de5639cc022ec23/internal/cache/cache.go#L313)
  owns reads, freshness, hit/miss counts, and debounced access writes. The same
  package owns year writes, clear, pruning, and close.
- [`cmd/server/benchmark_test.go:18`](https://github.com/calmcacil/sonarr-anime-bridge/blob/3f7a54d3c8cc7238ed611cb05de5639cc022ec23/cmd/server/benchmark_test.go#L18)
  measures a 600-show warm list request with realistic tags and relations.

The first implementation removes repeated JSON decoding. It deliberately keeps
one SQLite read per year access, preserving storage errors, statistics, retention,
and visibility of changed BLOBs. It still pays for BLOB transfer, byte comparison,
request slice copying, filtering, resolution, and response encoding. Lower
temporary allocation comes at the cost of retaining decoded objects between requests.

## Proposed changes

### Ownership and APIs

Add `internal/cache/decoded_year.go`; the memory cache belongs to each `Cache`
instance so writes, clear, prune, and close cannot bypass its lifecycle. Importing
`internal/anilist` for `Show` introduces no package cycle. No schema migration,
new external dependency, configuration switch, or output-response cache is needed.

Provide `GetDecodedYearContext(ctx, year)` and `PeekDecodedYearContext(ctx, year)`
with a shared result type containing `Shows`, `Fresh`, `Present`, and `DecodeErr`.
The separate returned `error` represents storage or cancellation failure.
`Present=true, DecodeErr!=nil` identifies a corrupt row; an absent row is not a
decode error. A valid `[]` produces an empty show slice with `Present=true`.
This distinction preserves HTTP error handling and cache-recovery decisions.

Keep raw `GetYearContext`, `PeekYearContext`, and `SetYearContext` for existing
callers. The decoded APIs invoke the existing raw read path exactly once per
call, using its current access-recording behavior. They do not issue an extra
read to obtain freshness or count an additional hit on a memory miss.

Move array validation/decoding into `cache.DecodeYearData`; retain the scheduler
helper as a delegating compatibility wrapper. Both paths must accept and reject
the same payloads, including old data with extra fields.

### Lookup, decoding, and capacity

Each retained entry contains the year, exact raw bytes, a private decoded slice,
accounted retained size, and LRU bookkeeping. Use named internal defaults of
eight retained years and 32 MiB of accounted payload memory per `Cache` instance.
These are initial bounds to validate against representative deployment data,
not a claim about the process's maximum RSS.

1. Check context and capture the cache's mutation generation before reading
   SQLite. Use the raw read to get bytes and current freshness/presence.
2. Under a short memory-state lock, compare the returned bytes with the resident
   entry using `bytes.Equal`. On a match, touch LRU and capture its private
   decoded slice, then release the lock before making the top-level return copy.
   Freshness always comes from SQLite.
3. On mismatch or absence, acquire a per-year decode gate with a context-aware
   wait. Recheck the memory entry after acquiring the gate. Decode that read's
   bytes if still needed, outside the global memory-state lock.
4. Admit only valid decoded data and only if the generation captured before
   the read is still current. Do not replace a newer admitted entry with a result
   from a read that overlapped a successful mutation. That older request may
   still return its privately decoded snapshot without admitting it.
5. Before admission, evict least-recently-used entries until both bounds hold.
   Oversized entries are decoded for the caller but are not retained. Capacity
   rejection is not an error. Do not retain invalid payloads or decode errors.

Account for retained raw-byte capacity, show backing arrays, strings, nested
tag/relation arrays, optional pointees, and entry overhead. Define and test the
accounting helper against the actual `anilist.Show` shape. Allocator rounding,
map overhead, in-flight reads/decodes, response buffers, and snapshots still used
by requests are outside this payload budget. Avoid retaining excess backing-array
capacity when practical. Do not promise immediate RSS reduction on eviction.

The gate serializes decodes of the same year without serializing different years.
Waiters recheck after admission, so concurrent reads of the same retained version
decode once. Oversized or invalid payloads can require repeated decoding.
Use reference-counted gate records and remove unused records; arbitrary years
must not create an unbounded registry. The leader owns no detached goroutine:
when it exits or is canceled, release the gate and allow another caller to retry.
Check cancellation before decode, after decode, and before returning. Go's JSON
decoder is not interruptible mid-call; document that existing limitation.

### Slice safety

Never return the resident show backing array. Return a top-level copy with
capacity equal to length; `Scheduler.Process` may compact or append to that
request-owned array. Make the same copy for prior-year winter inputs.

Nested slices and pointer fields are shared read-only. Document this restriction
on decoded APIs and `Process`, and audit all downstream processing for nested
mutation. Tests must verify that processing leaves the entire resident object
graph unchanged. If a future filter changes nested data, it must copy that data
first. A deep copy on every request would reintroduce much of the allocation
cost; changing every filter to accept immutable inputs is outside this feature.

### Mutation and lifecycle

Use a monotonically increasing in-process generation for admission fencing; do
not use second-resolution `fetched_at` as a version. Byte equality is the lookup
identity, including replacements within the same second and external BLOB edits.

- After a successful `SetYearContext`, advance generation and evict that year's
  resident entry before returning. Keep invalidation within the cache method,
  including writes from tests or future callers. Failed writes do not publish
  upstream data or invalidate a valid retained entry.
- After successful `ClearContext`, advance generation and remove all resident
  entries before reporting success, alongside existing metric/access resets.
  Clear failure does not claim success or publish an empty memory state.
- After a successful prune that deletes rows, advance generation and invalidate
  all resident entries. This intentionally avoids a schema change or a second
  query to identify deleted years; pruning is infrequent. Invalidate after the
  DELETE commits even if obtaining its affected-row count subsequently fails.
- Close marks memory state closed, advances generation, and drops entries.
  Reads that overlap shutdown cannot repopulate it. Preserve existing worker
  shutdown ordering and bounded waits.

Perform SQL work and decoding outside the memory-state lock. Invalidation and
admission compare/update generation under that lock. A read admitted between
SQL commit and invalidation is removed before the mutation method returns;
an older read completing after invalidation fails the admission check. Test
both orderings. Successful writes concurrent with clear may repopulate storage,
matching the existing clear contract; clear does not cancel scheduler fetches.

Every lookup still reads SQLite. Changes made outside this `Cache` instance,
including another process, are therefore detected through the exact BLOB;
deleted rows and read errors never fall back to a resident entry. A missing or
changed row removes its obsolete resident entry without changing the request's
storage outcome. No distributed invalidation protocol is required.

### Integration

- Replace the handler's raw-read-plus-decode pairs with the decoded read API,
  preserving `hit`, `miss`, `stale`, `invalid`, primary recovery, and prior-year
  backfill decisions. Keep the post-fetch reread and all existing timeouts.
- Use decoded peek in prewarm and the scheduler's fresh-cache validation.
  Successful fresh prewarm hydrates memory. After prewarm fetches a missing or
  stale year, perform a decoded peek to hydrate it; skip unnecessary hydration
  work if shutdown was requested. Prewarm remains after listen startup and does
  not record user hits.
- Keep `FetchAndStore` persisting JSON through `SetYearContext`. Do not publish
  the fetcher's mutable slice directly into memory; hydration occurs from the
  persisted row. The first post-write hydration decodes once, then reuses it.
- `/cache/clear` keeps calling `Cache.ClearContext`; health and `/cache/stats`
  remain persistence-based. Memory eviction never deletes SQLite rows.

## Testing and validation

Use controlled barriers/hooks for race orderings and an injectable decode
counter in package tests. Avoid wall-clock sleeps as coordination or timing
assertions as correctness tests.

| Product invariants | Evidence required |
| --- | --- |
| 1, 3, 4, 8 | Existing ordered HTTP pipeline matrix; alternate all seasons and both categories on one resident year; simultaneous winter/non-winter processing; complete before/after object-graph comparison; mapping swap and future-date boundary cases. |
| 2, 12, 16 | Decode-count tests for repeated and concurrent hits, changed bytes, same-second replacement, invalid arrays, valid empty arrays, LRU eviction, oversized bypass, capacity accounting, and bounded gate cleanup. |
| 5, 6, 7, 9 | Freshness boundary and calendar rollover tests; stale refresh success/failure; failed write retains prior data; coalesced fetch and post-fetch reread; old decode finishing after new write cannot re-admit old data. |
| 10, 11 | Warm then clear/prune; reads paused before and after commit/invalidation; successful clear followed by a late fetch; failed clear; pending access writes still protect recently accessed years. |
| 13, 14 | Restart with persisted fixture; fresh/fetched prewarm hydration without user hits; capacity-limited prewarm; cancel a decode waiter/leader; close during decode and no admission afterward. |
| 15 | Existing health/debug tests; exact hit/miss and last-hit behavior on memory hits and peeks; database close/read failure after warming; externally replaced/corrupted/deleted BLOBs cannot be masked by memory. |

Capture `BenchmarkListHit` on the baseline before implementation. Explicitly
hydrate before resetting its timer for the new steady-state measurement. Add
focused decoded-year benchmarks plus winter two-year and parallel HTTP cases;
separate first-load and steady-state results. Keep output correctness checked.

```sh
go test -run '^$' -bench BenchmarkListHit -benchmem -count=5 ./cmd/server
go test -race -run TestHandleListPipelineMatrix ./cmd/server
make check
```

Compare baseline and proposed implementation on the same hardware/toolchain
using `benchstat` and an allocation profile. Acceptance requires no JSON year
decoding for unchanged resident warm reads and a reduction in median warm-path
bytes/op and allocations/op. Target at least 50% lower bytes/op for the existing
600-show case; record results and explain any target shortfall before review.
Do not enforce machine-dependent latency percentages in CI or claim the earlier
Rust estimates as expected results. Record ns/op, first-load cost, retained heap
with one/two/eight representative years, and oversized bypass behavior.

Run `./testdata/native-regression.sh` for implementation review because slice
ownership and handler/scheduler integration affect the pipeline. Update the
service-baseline specs and architecture notes to describe the implemented cache
once it ships; proposed docs must not imply current runtime behavior changed.

## Risks and mitigations

- Shared nested data is safe only while processing stays read-only. Explicit
  ownership contracts, object-graph checks, and race tests protect that boundary.
- Resident memory grows even while temporary allocations shrink. Bounds and
  retained-heap measurements matter more than an assumed RAM saving.
- SQLite BLOB reads and comparisons remain linear in payload size. Profile the
  resulting path before proposing metadata-only reads or a full memory serving
  layer, which would require a separate storage-failure/coherence contract.

## Parallelization

Use one implementation agent. Cache lifecycle, admission fencing, request
ownership, and HTTP integration are tightly coupled; splitting these edits
across agents would add coordination without a useful independent workstream.
Implement cache primitives first, integrate callers second, then run race,
pipeline, and benchmark validation together in one feature branch/PR.
