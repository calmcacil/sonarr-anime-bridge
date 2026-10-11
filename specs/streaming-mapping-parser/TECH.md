# Tech Spec: Streamline Mapping Parsing

Status: implemented and validated in [PR #142](https://github.com/calmcacil/sonarr-anime-bridge/pull/142).

## Context

Research baseline (release `v2.15.2`): `ad495e9ae0124b0127a49e7ef9989f80fa0d7a43`.

- [`internal/mapping/anibridge.go:380`](https://github.com/calmcacil/sonarr-anime-bridge/blob/ad495e9ae0124b0127a49e7ef9989f80fa0d7a43/internal/mapping/anibridge.go#L380)
  streams zstd output into a JSON parser; this existing streaming boundary stays.
- [`internal/mapping/anibridge.go:522`](https://github.com/calmcacil/sonarr-anime-bridge/blob/ad495e9ae0124b0127a49e7ef9989f80fa0d7a43/internal/mapping/anibridge.go#L522)
  walks root properties, constructs final integer maps, and dispatches entries.
- [`internal/mapping/anibridge.go:603`](https://github.com/calmcacil/sonarr-anime-bridge/blob/ad495e9ae0124b0127a49e7ef9989f80fa0d7a43/internal/mapping/anibridge.go#L603)
  copies skipped values into `json.RawMessage`. Target extraction constructs
  `map[string]json.RawMessage`, then reparses relevant raw values into range maps.
- [`internal/mapping/mapping_test.go:189`](https://github.com/calmcacil/sonarr-anime-bridge/blob/ad495e9ae0124b0127a49e7ef9989f80fa0d7a43/internal/mapping/mapping_test.go#L189)
  covers parsing and scope selection; a separate tie test checks lowest-ID choice.

This spec removes intermediate raw JSON where practical. It does not promise
that Go's token decoder is allocation-free or faster for every input. Keep the
loader protocol separate from [refresh reuse](../mapping-refresh-reuse/TECH.md).

## Implementation

### Preserve the streaming and publication boundaries

Keep `parseAnibridge`, zstd streaming, `limitReader`, and the final immutable
`AnibridgeMapping`. Factor parsing helpers into `internal/mapping/parse.go` if
that makes the token traversal readable. Start with `encoding/json`; introduce
no custom lexer, unsafe code, or external JSON library in this feature.

Do not retain the complete decompressed input, use `io.ReadAll` for JSON, or
construct a full generic tree. Build the two final integer maps directly.
Keep current error context identifying parsing stage/source entry, along with
dataset metadata, duration, counts, and safe loader error handling.

### Skip unused data without copying its raw representation

Replace `skipValue`'s `RawMessage` decoding with token traversal that consumes
exactly one value, including nested objects/arrays. Use an explicit delimiter
stack to avoid recursive application-stack growth. Check context during long
traversals and let `encoding/json` enforce syntax rules. The size-limited reader
must wrap all JSON reads, including ignored data.

Token decoding still allocates strings for ignored string values. The intended
saving is avoiding complete raw-value buffers and generic maps, not eliminating
all allocations. Do not tighten root/trailing-input acceptance as an incidental
optimization: the current parser stops after the first root object's close.
Any input-policy hardening must be a separately reviewed change.
`Decoder.Token` does not enforce `Decoder.Decode`'s nesting limit. The traversal
therefore preserves the existing 10,000-container limit per root property value,
including the enclosing source/target or metadata objects where applicable.
This reproduces the original decoder boundary rather than adding an entry cap.
Check cancellation before and after token reads as well as between nested
values. Standard decoder calls and an underlying blocked read are not inherently
interruptible; retain existing timeout behavior and document this limitation.

### Extract compact target summaries

For a recognized source ID, walk its target object. Parse descriptors directly;
skip irrelevant or invalid descriptors using the traversal helper. Relevant
targets produce a compact summary of TVDB ID, scope preference, source episode
count, and any semantic error. Consume every relevant candidate before selecting
the winner, preserving validation of non-winning candidates.

Current target decoding uses a map: duplicate target descriptors use the last
raw value, so an earlier semantic error may be overwritten by a valid later
value. Retain one summary per relevant descriptor until the source entry ends,
then select the winner and surface errors from surviving summaries. Do not fail
early on a semantic error that the baseline would discard through overwrite.
JSON syntax and reader errors still fail immediately. Clear entry summaries
before moving to the next source ID; unrelated targets are never retained.

Range counting likewise needs compact per-target state for duplicate range keys.
Validate value types as the baseline does, retain the unique source keys needed
to reproduce its count, and sum using the existing positive integer/range rules.
Do not add counts twice for repeated keys or count destination ranges. Keep
integer-width and overflow behavior compatible pending a separate hardening
decision. Objects and `null` retain the current distinctions.

Duplicate root source entries update a lookup only when the new entry resolves
successfully. An unresolved later entry does not erase a previous valid mapping;
a failing relevant entry still fails the dataset. Preserve informational `$meta`
handling rather than treating it as an ordinary required mapping entry.
Metadata extraction reports semantic type errors separately from fatal syntax,
depth, reader, limit, and cancellation errors. Usable datasets with semantic
metadata problems still load with the existing warning; fatal errors return no
partial mapping.

Per-entry summaries and unique-range keys are the necessary temporary state.
Their size is bounded by the entry/input limits, not a new arbitrary entry-count
cap. Do not retain them in `AnibridgeMapping` or across parser invocations.

### Integrate without changing refresh policy

Keep parser entry points compatible with current file and downloaded-byte
callers. Failed parsing returns no usable partial mapping and leaves atomic
file writes/resolver swaps under the loader's existing control. If refresh reuse
is implemented first, parser benchmarks must explicitly force changed content
so an unchanged-source fast path cannot hide parsing work.

## Testing and validation

Retain a test-only reference implementation of the inspected baseline parser
during the change. Differential tests compare success/failure, both final maps,
statistics, and sorted keys. Compare stable error categories/context, not
incidental standard-library wording or log durations.

| Product invariants | Required evidence |
| --- | --- |
| 1, 2, 3 | Existing fixtures plus permutations of candidate order; separate namespaces; invalid IDs/descriptors; MAL-first resolution; all scope, episode-count, and lowest-ID ties. |
| 4, 5 | Single/closed/open ranges, null/empty objects, invalid ranges/types, duplicate range keys, duplicate target keys with valid/invalid earlier/later values, duplicate roots with resolved/unresolved later values. |
| 6, 7 | Root shape, truncated JSON/zstd, escaped keys/strings, scalar and nested ignored values, metadata failures, current trailing-input behavior, and reader errors. |
| 8, 9, 10 | Size-boundary fixtures including large ignored fields, context cancellation during nested skipping/range scanning, concurrent parser calls under race detection, no partial result on failure, and loader fallback tests. |
| 11 | Existing full HTTP pipeline and rootless runtime/build gates; no config/schema/protocol changes. |

Add deterministic fixtures with many irrelevant targets, long ignored values,
multiple scopes, and representative dataset counts. Fuzz differential parsing
with bounded input sizes and seeded duplicate-key cases. Keep the old parser
test-only; production must have one active parsing implementation.

```sh
go test -race ./internal/mapping ./internal/scheduler ./cmd/server
go test -run '^$' -bench BenchmarkMappingParse -benchmem -count=5 ./internal/mapping
go test ./internal/mapping -run '^$' -fuzz FuzzMappingParserParity -fuzztime=30s
go test -race -run TestHandleListPipelineMatrix ./cmd/server
make check
```

The benchmark and fuzz target are implemented. Compare baseline and new
parsers using `benchstat` on the same machine/toolchain. Record pure JSON and
zstd-plus-JSON time, bytes/op, allocations/op, and peak/retained heap separately.
Acceptance requires exact fixture parity and lower allocated bytes/op on the
representative mapping workload, with no repeatable statistically significant
time regression. If token traversal does not meet this, refine or defer the
optimization; do not declare success solely because the code appears to stream.
No machine-dependent percentage is a CI correctness gate.

Run `./testdata/native-regression.sh` for implementation review because mapping
selection affects ordered list results. Update the service-baseline docs and
[TODO.md](../../TODO.md) when verified implementation lands.

## Implementation evidence

### Correctness

[`parse.go`](../../internal/mapping/parse.go) is the only production JSON parser.
[`parse_reference_test.go`](../../internal/mapping/parse_reference_test.go)
freezes the parser from `deb5dbe` for differential tests and benchmarks.
[`parse_test.go`](../../internal/mapping/parse_test.go) compares acceptance,
both lookup maps, statistics, sorted keys, source context, and stable error
categories. The reference map's iteration order can choose different failing
descriptors when multiple errors survive; tests compare the source and stage
rather than that descriptor or incidental standard-library wording. Malformed
metadata may fail at a different root-traversal stage because token traversal
has already consumed part of the value.

Coverage includes separate namespaces, invalid descriptors, all 24 permutations
of four competing scopes, closed/open/single ranges, null values, duplicate
ranges/targets/sources, overwritten semantic errors, surviving non-winning
errors, integer overflow compatibility, escapes, unrelated nested values,
metadata warnings and fields, first-root/trailing-input acceptance, malformed
JSON/zstd, reader failure, cancellation, and concurrent invocations. The
18,000-entry benchmark fixture also passes exact map parity. Deep fixtures
exercise the original value-depth limit at and around 10,000 containers,
including an over-depth earlier duplicate that must remain a fatal error.
The decoded-size test consumes an ignored value beyond the real 250 MiB limit
without constructing a large fixture; shorter fixtures test exact boundaries.
Loader coverage proves that a semantic parse error in a non-winning candidate
preserves the active mapping, valid cached bytes, and prior metadata.

The final 30-second differential fuzz run passed 448,030 executions with a
64 KiB input cap and seeded duplicate-key cases. The full race suite and the
deterministic HTTP pipeline matrix pass. The native regression passes against
`v2.15.2`, comparing complete ordered responses for all seasons and both
categories, health, validation, methods, authentication, cache clearing, and
shutdown. An initial attempt timed out fetching the released server's prior
AniList year; the subsequent run completed all assertions.

`make check` passes formatting, module validation, vet, lint, the full race suite,
both Linux cross-builds, govulncheck, workflow validation, and documentation links.
Its Docker target cannot access the local daemon socket, including outside the
workspace sandbox. [PR #142 CI](https://github.com/calmcacil/sonarr-anime-bridge/actions/runs/38136808727)
validates implementation commit `50e62e6`, passing native amd64/arm64 container
builds, version checks, rootless runtime smoke, and image vulnerability scans,
plus quality, race tests/coverage, dependency review, govulncheck, and CodeQL for
Go/Python/Actions. `Required` and `PR title` pass. The active main ruleset enforces
those checks and CodeQL results. No dependency, configuration, database, HTTP
protocol, or loader policy changes are introduced.

### Benchmarks

Measured with Go 1.27.2 on Linux arm64 with four available CPUs. Reference and
streaming implementations run sequentially on the same machine in the same test
binary, with six samples per mode and no other local build/test work running.
The deterministic fixture contains 18,000 sources, split equally between MAL
and AniList, with two TVDB scopes and an ignored target per source. Two documents
alternate selected TVDB IDs. Pure JSON and zstd-plus-JSON benchmarks call the
parser directly on every iteration; no source-reuse path can hide parsing work.
Fixture construction/compression and logger setup are outside timed sections.
Zstd reader creation and closing are included in the compressed measurement.

| Mode | Reference time | Streaming time | Reference bytes/op | Streaming bytes/op | Change |
| --- | --- | --- | --- | --- | --- |
| JSON | 91.51 ms | 42.67 ms | 26.75 MiB | 6.50 MiB | 53.37% less time; 75.70% fewer bytes. |
| Zstd | 94.91 ms | 44.58 ms | 36.65 MiB | 16.40 MiB | 53.03% less time; 55.25% fewer bytes. |

`benchstat` reports significant improvements in both time and allocated bytes
(`p=0.002`, six samples). Allocation counts also fall by about 23%. These results
describe parsing this fixture, not production network latency or `/list` speed.
Token traversal can still allocate strings for ignored values, and temporary
range/target state is bounded by the current entry and input limits.

The changed-local loader benchmark alternates compressed file contents after
priming reuse, forcing a real parse on every measured refresh. It measures
45.70 ms and 17.15 MiB/op, including source fingerprinting, metadata and key
snapshot work; fixture edits remain outside timing.

A separate opt-in heap report samples `HeapAlloc` every millisecond and performs
a GC while retaining the result. Three-run medians of additional heap over the
pre-parse baseline are:

| Mode | Reference sampled peak | Streaming sampled peak | Reference retained | Streaming retained |
| --- | --- | --- | --- | --- |
| JSON | 3.12 MiB | 2.64 MiB | 0.58 MiB | 0.56 MiB |
| Zstd | 21.88 MiB | 16.38 MiB | 0.59 MiB | 0.60 MiB |

These are sampled additional heap estimates, not a precise process peak or RSS.
Fixture memory is present before the baseline; runtime caches, GC timing and
sampler overhead can affect the deltas. Retained maps are equivalent; the saving
is temporary parser work. There is no machine-dependent CI timing or heap gate.

Raw [combined parser samples](benchmarks/parser.txt), normalized
[baseline](benchmarks/baseline.txt) and [streaming](benchmarks/streaming.txt)
samples, [benchstat comparison](benchmarks/benchstat.txt),
[changed-refresh samples](benchmarks/changed-refresh.txt), and
[heap samples](benchmarks/heap.txt) accompany this implementation. Normalized
sample files remove only the implementation label from benchmark names to align
the two rows for comparison. Reproduce measurements with:

```sh
go test -run '^$' -bench BenchmarkMappingParse -benchmem -count=6 ./internal/mapping
go test -run '^$' -bench 'BenchmarkMappingRefresh/ChangedLocal$' \
  -benchmem -count=6 ./internal/mapping
MAPPING_HEAP_REPORT=1 go test -run TestMappingParserHeapReport -count=3 -v ./internal/mapping
go run golang.org/x/perf/cmd/benchstat@v0.0.0-20261009192801-be2c69fb417e \
  specs/streaming-mapping-parser/benchmarks/baseline.txt \
  specs/streaming-mapping-parser/benchmarks/streaming.txt
```

`MAPPING_HEAP_REPORT` is a test-only switch, not an application setting. Release
remains deferred until the optimization batch and required validation in
[TODO.md](../../TODO.md) are complete.

## Risks and mitigations

- Duplicate-key behavior prevents a fully stateless winner-only reduction.
  Compact summaries plus differential fixtures preserve existing semantics.
- Token traversal can trade fewer large copies for more small allocations.
  Measure both time and allocations before retaining the rewrite.
- Dropping validation for irrelevant/non-winning content can change acceptance.
  Keep syntax checking throughout and characterize semantic boundaries explicitly.

## Parallelization

Use one implementation agent. Skipping, duplicate handling, candidate selection,
and validation share decoder state and one source file. Implement helpers and
differential tests together, then benchmark and integrate in one branch/PR.
