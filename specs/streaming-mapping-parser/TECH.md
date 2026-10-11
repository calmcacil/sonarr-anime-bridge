# Tech Spec: Streamline Mapping Parsing

Status: proposed; implement after review of [PRODUCT.md](./PRODUCT.md).

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

## Proposed changes

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

The benchmark and fuzz target are proposed additions. Compare baseline and new
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
