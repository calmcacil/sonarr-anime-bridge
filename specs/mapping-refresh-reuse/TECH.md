# Tech Spec: Reuse Unchanged Mappings

Status: proposed; implement after review of [PRODUCT.md](./PRODUCT.md).

## Context

Research baseline: `4b532b7ea516d4c13e3a77ee91455f951800499a`. This feature is
independent of [decoded-year-cache](../decoded-year-cache/TECH.md) and the
[streaming parser](../streaming-mapping-parser/TECH.md).

- [`internal/mapping/anibridge.go:93`](https://github.com/calmcacil/sonarr-anime-bridge/blob/4b532b7ea516d4c13e3a77ee91455f951800499a/internal/mapping/anibridge.go#L93)
  exposes stateless `LoadOrFetch`. Matching ETags and matching download MD5s
  still cause the cached file to be decompressed and parsed.
- [`internal/mapping/anibridge.go:305`](https://github.com/calmcacil/sonarr-anime-bridge/blob/4b532b7ea516d4c13e3a77ee91455f951800499a/internal/mapping/anibridge.go#L305)
  downloads bounded compressed bytes and validates MD5 only when supplied by
  the upstream header. The sidecar stores ETag, URL, MD5, and key snapshots.
- [`internal/scheduler/scheduler.go:170`](https://github.com/calmcacil/sonarr-anime-bridge/blob/4b532b7ea516d4c13e3a77ee91455f951800499a/internal/scheduler/scheduler.go#L170)
  discards returned metadata and swaps every successful loader result into
  the atomic resolver. Initial load and background refresh use this path.
- [`internal/mapping/mapping_test.go:430`](https://github.com/calmcacil/sonarr-anime-bridge/blob/4b532b7ea516d4c13e3a77ee91455f951800499a/internal/mapping/mapping_test.go#L430)
  covers ETag short-circuiting of downloads; related tests cover source changes,
  MD5 matches, corrupt downloads, fallback, and metadata persistence.

## Proposed changes

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

`BenchmarkMappingRefresh` is a proposed benchmark to add during implementation.
Update service-baseline docs and [TODO.md](../../TODO.md) when verified work lands.

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
