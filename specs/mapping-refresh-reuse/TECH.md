# Tech Spec: Reuse Unchanged Mappings

Status: implementation complete; final pull-request validation is pending.

## Context

Research baseline (release `v2.15.2`): `ad495e9ae0124b0127a49e7ef9989f80fa0d7a43`. This feature is
independent of [decoded-year-cache](../decoded-year-cache/TECH.md) and the
[streaming parser](../streaming-mapping-parser/TECH.md).

- [`internal/mapping/anibridge.go:93`](https://github.com/calmcacil/sonarr-anime-bridge/blob/ad495e9ae0124b0127a49e7ef9989f80fa0d7a43/internal/mapping/anibridge.go#L93)
  exposes stateless `LoadOrFetch`. Matching ETags and matching download MD5s
  still cause the cached file to be decompressed and parsed.
- [`internal/mapping/anibridge.go:305`](https://github.com/calmcacil/sonarr-anime-bridge/blob/ad495e9ae0124b0127a49e7ef9989f80fa0d7a43/internal/mapping/anibridge.go#L305)
  downloads bounded compressed bytes and validates MD5 only when supplied by
  the upstream header. The sidecar stores ETag, URL, MD5, and key snapshots.
- [`internal/scheduler/scheduler.go:170`](https://github.com/calmcacil/sonarr-anime-bridge/blob/ad495e9ae0124b0127a49e7ef9989f80fa0d7a43/internal/scheduler/scheduler.go#L170)
  discards returned metadata and swaps every successful loader result into
  the atomic resolver. Initial load and background refresh use this path.
- [`internal/mapping/mapping_test.go:430`](https://github.com/calmcacil/sonarr-anime-bridge/blob/ad495e9ae0124b0127a49e7ef9989f80fa0d7a43/internal/mapping/mapping_test.go#L430)
  covers ETag short-circuiting of downloads; related tests cover source changes,
  MD5 matches, corrupt downloads, fallback, and metadata persistence.

## Implementation

### A loader with explicit source ownership

Add `internal/mapping/loader.go` with one scheduler-owned `Loader`, used for
startup, retries, and periodic refresh. It retains only the latest successfully
loaded mapping and its source identity. Keep the public stateless `LoadOrFetch`
wrapper for existing callers by invoking the shared implementation without
previous state; avoid maintaining two divergent fetch/fallback algorithms.

Use a result containing mapping, metadata, and disposition (`loaded`, `reused`,
or `fallback`). An error still represents an unsuccessful load; a successful
local fallback carries its existing degraded diagnostic. The scheduler installs
new successful mappings, leaves an identical reused pointer in place, and keeps
the current resolver on errors. Adapt the injectable loader seam in scheduler
tests. Do not introduce a package-global cache or put HTTP/file I/O in `Resolver`.

Source identity includes the validated effective path and configured URL, a
SHA-256 digest of the actual compressed bytes that were parsed, and the metadata
associated with that source. An ETag, MD5 sidecar, size, or modification time
alone is not identity. SHA-256 here is a content fingerprint, not authenticity
verification; existing upstream MD5 and URL validation remain separate.

### Content checks and reuse

Preserve the existing remote decision tree: matching ETag selects local cache;
HEAD failure continues to GET; download failure/invalid download can use local
fallback; a changed URL bypasses an old source's cache. Apply reuse at the point
where that decision tree has selected and validated a source.

- For a selected local file, open and hash the compressed contents in bounded
  chunks, checking context between reads. Hash even if size and timestamp match.
  On a digest/path/URL match with previously parsed state, return the same mapping
  without starting zstd or JSON decoding. A successful HEAD plus verified local
  identity is `reused`; an upstream failure plus verified cache identity remains
  a fallback, not a successful upstream freshness check.
- For changed local content, parse the same opened file from the beginning.
  Hash the bytes consumed by parsing and drain the remaining compressed stream
  to establish its complete digest. Confirm it agrees with the initial hash
  before accepting state. File replacement after open uses the opened snapshot;
  an in-place write that changes content during inspection must fail the
  consistency check and retry on a later refresh. Do not publish mixed data.
- For downloaded content, perform existing size/checksum validation, then hash
  the bounded byte slice. Compare against parsed state only for the same path
  and URL. Equal bytes may reuse the mapping even with missing MD5 or changed
  ETag; refresh metadata and preserve existing key-snapshot semantics. If the
  local file is missing or differs, persist the validated download before
  reporting successful reuse. Reusing parsing must not skip cache repair or
  hide a failed write. Skip rewriting only a verified identical local file.
- Changed downloads use the existing parser, atomic mapping-file write, and
  best-effort sidecar persistence. Store new reusable state only after successful
  load completion. A failed mapping-file write does not advance reusable state.

The hash is computed from actual selected bytes even when a checksum header is
absent. No new sidecar field is required: restart always parses. Sidecar write
failure remains a warning and can cause an extra download next time. A missing
or corrupt local source cannot take the local reuse branch merely because
the loader remembers a mapping; resolver fallback on error remains scheduler-owned.

Keep remote compressed/decoded size limits and existing local-file acceptance
rules. Hashing a local file uses constant-sized scratch storage rather than
reading it all into a new byte slice or creating a retained compressed copy.
Changed-file checks can read compressed bytes more than once; measure this cost.

### Concurrency, publication, and logs

Serialize loader operations with a context-aware admission gate; canceled
waiters stop waiting. The gate protects source-state decisions without blocking
resolver lookups, and releases on every exit. Avoid detached work and holding
the resolver's reader path behind I/O. Reuse retains a single source state;
failed changes do not create additional retained versions.

The scheduler also serializes loading through resolver publication with a
context-aware gate. This prevents a concurrent caller from installing an older
completed result after a newer load. These gates do not participate in lookups.
The package-private parser and atomic-write seams support deterministic parse
counters, mutation barriers, and persistence failures in tests.

Preserve cancellation checks before publication and existing shutdown coupling.
Use existing structured task/outcome fields, adding an explicit reuse reason
such as `unchanged_content`; do not emit a parse-success event when no parse ran.
Do not expose configured URLs through recoverable error wrapping. Keep active
mapping counts, key-change diagnostics for real changes, and fallback warnings.

## Testing and validation

| Product invariants | Required evidence |
| --- | --- |
| 1, 2, 7, 12 | Injected parse counter: startup parses once; matching HEAD/local digest parses zero additional times; identical GET without MD5 reuses; restart parses; changed sources retain only current state. |
| 3, 4, 5 | Same-size/preserved-time file edit, atomic replacement, changed URL/path, edited/missing sidecar, missing file, read failure, corrupt file, and barrier-controlled file changes between hashing and parsing. |
| 6, 8 | Existing checksum, limits, allowlist/redirect, HEAD-failure, invalid-download, fallback, atomic-write, and metadata tests; failed write cannot publish candidate state; source fallback never claims fresh verification. |
| 9, 10, 11 | Scheduler/HTTP health and resolver retry tests; concurrent reads during refresh; canceled admission and parsing; subsequent retry succeeds; shutdown leaves no stuck gate; safe reuse/fallback/changed-load logs. |

Add focused benchmarks for unchanged local refresh, changed local refresh,
identical GET, and initial load. Use fixed compressed fixtures and controlled
HTTP responses; exclude variable network latency. Compare the baseline and new
loader on the same toolchain/hardware with `benchstat`. Acceptance requires zero
zstd/JSON parse invocations on verified reuse, bounded scratch memory, and lower
unchanged-refresh allocations. Report hash time and changed-load overhead;
do not enforce machine-dependent timing percentages in CI.

```sh
go test -race ./internal/mapping ./internal/scheduler ./cmd/server
go test -run '^$' -bench BenchmarkMappingRefresh -benchmem -count=5 ./internal/mapping
make check
```

`BenchmarkMappingRefresh` and `BenchmarkMappingFingerprint` are implemented in
[`refresh_bench_test.go`](../../internal/mapping/refresh_bench_test.go).
The service-baseline docs and [TODO.md](../../TODO.md) track the implemented
architecture and validation state.

## Implementation evidence

### Correctness

The loader tests count parser invocations: startup parses once, verified local
refresh and identical GET add zero parses, and a new loader parses after restart.
They cover same-size/preserved-time edits, atomic replacement, changes between
hashing and parsing, URL/path changes, removal/corruption/edits of metadata,
missing/corrupt/unreadable local files, invalid downloads, bad checksums, cache
repair, persistent-write failure and retry, best-effort sidecar writes,
concurrent callers, canceled admission/parsing, and interrupted fingerprints.
Existing `LoadOrFetch`, fetch, URL validation, parser, and metadata tests use the
same implementation through the stateless wrapper.

Scheduler coverage checks loaded/reused/fallback diagnostics, continued reads
during refresh, canceled publication and subsequent retry, unchanged refresh
cadence, unloaded recovery, and shutdown while a mapping load is waiting.

Local race tests pass. `./testdata/native-regression.sh` passes against `v2.15.2`
for complete ordered responses in every season and both categories, health,
validation, allowed methods, admin authentication, cache clearing, and shutdown.
`make check` passes formatting, module validation, vet, lint, the full race suite,
both Linux cross-builds, govulncheck, workflow validation, and documentation links.
Its final Docker target cannot run locally because the Docker socket is not
accessible to this account. Both native container platforms must pass PR CI
before merge.

### Benchmarks

Measured with Go 1.27.2 on Linux arm64, four Neoverse-N1 vCPUs. Baseline application
code is `2bf5c2be9c9a0092bb5a142979923e85dcb59e00`; candidate application code is
`a858f55`. Baseline and candidate run sequentially on the same machine with
`-benchmem -count=6`, without other local build/test work running.

The fixed fixture has 18,000 source entries split equally between MAL and
AniList, two TVDB scopes and an irrelevant target per entry, and persisted key
snapshots for both namespaces. A deterministic HTTP transport removes network
latency while preserving the loader's HEAD/GET decision tree. Initial load uses
an existing valid local cache and a new loader each iteration; changed local
refresh alternates valid payloads. Setup, priming, and fixture edits are excluded
from refresh timing. Identical GET omits checksum headers and includes sidecar
persistence. The baseline uses the original stateless `LoadOrFetch` with the
same harness and fixture; it has no reuse path.

| Scenario | Baseline time | Reuse time | Baseline bytes/op | Reuse bytes/op | Interpretation |
| --- | --- | --- | --- | --- | --- |
| Unchanged local | 95.465 ms | 1.727 ms | 37.326 MiB | 0.703 MiB | 98.19% less time; 98.12% fewer allocated bytes; zero additional parses. |
| Changed local | 95.25 ms | 96.92 ms | 37.32 MiB | 37.39 MiB | 1.75% added time and 0.19% added bytes for content verification. |
| Identical GET | 98.836 ms | 4.718 ms | 40.164 MiB | 2.042 MiB | 95.23% less time; 94.92% fewer allocated bytes; zero additional parses. |
| Initial local load | 95.78 ms | 96.30 ms | 37.33 MiB | 37.40 MiB | No statistically significant time difference (`p=0.065`); 0.19% added bytes. |

`benchstat` reports significant unchanged-local and identical-GET savings
(`p=0.002`, six samples) and the changed-local time cost (`p=0.009`). Unchanged
local allocations fall from approximately 468,676 to 101 per refresh. A file-only
fingerprint costs 33.40 us and 33,096 B/op with 32 KiB scratch storage; the fixture
is approximately 36 KiB compressed. Metadata decoding remains part of the
unchanged-local cost, and GET reuse still downloads and persists metadata.
These results describe the deterministic fixture, not production network
latency or a `/list` speedup. There is no machine-dependent CI timing gate.

Raw [baseline](benchmarks/baseline.txt), [reuse](benchmarks/reuse.txt), and
[benchstat comparison](benchmarks/benchstat.txt) are committed with this change.
Reproduce the candidate measurements with:

```sh
go test -run '^$' -bench 'BenchmarkMappingRefresh|BenchmarkMappingFingerprint' \
  -benchmem -count=6 ./internal/mapping
go run golang.org/x/perf/cmd/benchstat@v0.0.0-20261009192801-be2c69fb417e \
  specs/mapping-refresh-reuse/benchmarks/baseline.txt \
  specs/mapping-refresh-reuse/benchmarks/reuse.txt
```

Release remains deferred until the optimization batch in `TODO.md` is complete.

## Risks and mitigations

- Local content hashing retains disk work. This intentionally preserves detection
  of same-size/same-time edits while removing the larger decode/parse work.
- An upstream ETag is not proof of local content. Digest actual bytes and retain
  source association; keep MD5 validation and URL restrictions unchanged.
- Successful mapping publication and best-effort sidecar persistence are distinct.
  Tests must not make a missing sidecar prevent an otherwise usable mapping load.

## Parallelization

Use one implementation agent: source identity, fallback, persistence, and
scheduler publication are coupled. Implement the loader and tests first, then
integrate the scheduler and run the full checks in one feature branch/PR.
