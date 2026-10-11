# AniList Batching Measurements

Measured on 2026-10-11 using Go 1.27.2, Linux arm64, four ARM Neoverse-N1
vCPUs. [metadata.json](metadata.json) records the source and input fingerprints.
The candidate implementation is `9271eb8156f5f058be09967cf19d4d1af115d419`,
based on `89bfe4eae662e4564162760076f9cab067d2bb0f`.

## Inputs and Reference Variants

The committed [2025 capture](../../../internal/anilist/testdata/year-2025.json)
contains 482 ordered records from the original single-page query. Its raw file
SHA-256 is `19e3266b2ba70bd3c34090394f88d33e685df150d4a1894710d8c6df71db8632`.
Tests compare every decoded Show field and nested array, not only IDs. Larger
fixtures repeat records, preserving duplicates; response fixtures are prepared
outside timed benchmark loops.

Three controlled-latency variants use the same server measurement harness:

- Original reference: the research revision with its 700 ms pacing.
- Compliant reference: that revision with only `rateLimitDelay` changed to 2300 ms.
- Candidate: four-page batches with shared 2300 ms pacing.

No variant sends live AniList requests. The transport sleeps for the specified
50 or 500 ms, serves identical page contents, and retains actual dispatch times.
Thirty scenarios cover 1/4/5/10/12 pages, both latencies, cold list requests,
two simultaneous distinct-year misses, and sequential two-year prewarm. Each
cell has six samples, giving 540 complete operations across three variants.
Independent scenario clients represent isolated process/upstream budgets and
run in parallel; they do not model many independent clients sharing one IP.

## Retained Results

- [Latency comparison](latency-comparison.md) reports median and nearest-rank
  p95 (maximum with six samples), request counts and the compliant comparison.
- `latency/{old,compliant,batched}/` retains every completion, dispatch time,
  output digest and elapsed time. Every scenario succeeds and has matching
  list digests across revisions.
- Per-run controlled network work is `attempts * fixed latency`. The minimum
  pacing wait is `(attempts - 1) * (admission interval - fixed latency)` because
  these successful, retry-free requests do not overlap in the transport.
  The remaining elapsed time includes processing, SQLite, scheduling, timer
  overshoot and contention. These are derived lower bounds, not a direct
  measurement of time asleep; raw dispatch times allow independent inspection.
- [Warm benchstat](warm-benchstat.txt) compares the serial, Winter and parallel
  warm benchmarks with six samples and allocation reports. No >5% regression
  appears. Warm processing code is unchanged; lower observed timings are not
  attributed to batching. Integration tests establish zero additional upstream
  calls while a shared cooldown is active.
- [Fetch benchstat](fetch-benchstat.txt) includes empty, one-record, 482-record,
  600-record and 5,000-record years, with requests, response bytes, CPU time,
  allocated bytes and allocation counts. Pacing is disabled in both variants
  for this measurement. Representative CPU/byte increases remain below 10%.
- [Native release replay](native-regression.json) records exact ordered
  season/category responses, endpoints, persistence, clear and shutdown against
  `v2.15.2`, with 100 HTTP timing samples per selected warm route. Its checksummed
  input manifest is retained in the report; the full local replay lives at
  `/tmp/anilist-batching-native-final`.

The representative 482-record fetch drops from ten requests to three; the
600-record fetch drops from twelve to three. Empty/one-record years have a fixed
four-alias decode cost: about 8-9 microseconds and 1.5-2.8 KiB extra per fetch.
That overhead is reported rather than averaged away.

Two one-page years still need two requests. At 50 ms latency their complete time
increases from about 0.76 s to 2.36 s against the old, excessive rate; it matches
the compliant reference. Five-page two-year operations also rise slightly
against the old client because four batches require three compliant gaps. All
ten-/twelve-page operations improve over both references, and every candidate
median satisfies its specified comparison. The slowest candidate run finishes
in about 12.1 s, within existing deadlines.

Initial samples that used unsupported request years were rejected and excluded.
CPU samples that accidentally retained legacy pacing, and warm samples collected
while the full build/lint gates were running, were also excluded.

## Reproduction

Create a temporary reference checkout with `git archive` of the reference
revision. Copy these current test inputs into it:

```text
cmd/server/anilist_performance_test.go
internal/anilist/fetch_benchmark_test.go
internal/anilist/testdata/year-2025.json
```

The shared fetch benchmark's `client.interval = 0` must become
`client.limiter = rate.NewLimiter(rate.Inf, 1)` in the reference test, adding the
`golang.org/x/time/rate` import. For reference CPU measurements also set
`rateLimitDelay` to zero in the temporary client: its throttle resets the limiter
on every attempt. Restore 700 ms for original controlled-latency measurements,
or 2300 ms for the compliant reference. Do not modify the production checkout's
pacing. Warm benchmarks never call that client.

Run sequential CPU/warm measurements on each revision:

```sh
go test -run '^$' -bench 'BenchmarkList(Hit|WinterHit)' -benchmem -count=6 ./cmd/server
go test -run '^$' -bench BenchmarkFetchYear -benchmem -count=6 ./internal/anilist
benchstat warm-before.txt warm-after.txt
benchstat fetch-before.txt fetch-after.txt
```

Run controlled timing on each variant, retaining a separate output directory:

```sh
ANILIST_PERFORMANCE_OUTPUT=/tmp/anilist-performance go test -run '^TestAniListPerformance$' -parallel=32 -timeout=12m ./cmd/server
```

These thresholds are reviewed release criteria, not machine-dependent CI
assertions. [Hosted CI evidence](ci-validation.json) records the successful
implementation run, including both native container platforms, rootless smoke
and image scans. [Live main protections](main-ruleset.json) were also verified.
Manual release review remains a separate gate; no release publication occurred
during these measurements.
