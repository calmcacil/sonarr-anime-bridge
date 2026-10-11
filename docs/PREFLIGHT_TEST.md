# Preflight Tests

Run `make check` before creating a PR. For filtering, season splitting, winter
merge, resolution, sorting, caching, or pipeline changes, also compare the
working binary with a release through native regression.

## Local Checks

```sh
make check
```

This runs formatting, module consistency, vet, lint, race tests, native Linux
amd64/arm64 builds, vulnerability scanning, workflow/shell checks, Python tooling
regression tests, and documentation link validation. It does not build or start
containers. CI retains image builds and runtime validation; `make check-images`
adds explicit image checks when needed. See [CI and releases](CI_RELEASES.md).

The native comparison supplements hermetic Go tests. Existing fixtures cover
exact IDs, titles, ordering, winter deduplication, every season, both categories,
filter boundaries, cache ownership/coherence, retries, cancellation, and mapping
fallback. Keep those tests even when release and working outputs match.

## Native Release Comparison

```sh
make regression
# Equivalent, with CLI options:
./testdata/native-regression.sh --ref-tag v2.15.2
```

Default reference: latest published stable release. Default data: the previous
calendar year, plus the preceding December for WINTER. The first run captures
these inputs once using the reference service. Both binaries then run against
identical year arrays and compressed mapping bytes. This prevents differences
from independent AniList fetches or mapping updates during a comparison.

The current releases have no downloadable native assets. Without a supplied
reference binary, the runner exports and builds the exact selected tag, then
caches the verified host binary. It never checks out or changes the working
branch. A missing local tag is fetched into an isolated temporary repository.
The working binary is rebuilt from the current checkout, including uncommitted
code, each run unless explicitly supplied.

The runner checks:

- Complete ordered responses for ALL, WINTER, SPRING, SUMMER, and FALL, for both
  `series` and `series-new`. IDs, titles, duplicates, and ordering must match.
- Component readiness, query validation, allowed methods, HEAD requests, and
  debug endpoint authentication.
- Cache persistence and identical responses after restarting each binary.
- POST cache clear, empty stats afterward, and bounded successful shutdown.
- No upstream year fetch during replay. Cached inputs are given fresh test-cache
  timestamps; the snapshot's original capture time stays in its manifest.

A nonempty full year and nonempty ALL/series response are required. Missing,
partial, empty, malformed, or mismatched data cannot pass as two equal empty
responses. Legitimately empty individual seasons/categories and preceding
December are supported.

First capture needs Go (unless both binaries are supplied), Git and GitHub CLI
for default release resolution/source builds, and upstream network access.
Python 3.12+ is required. No curl, jq, Docker, or Git worktree is required by the
runner. Host-compatible supplied binaries must be executable; the reference
binary's `--version` must match its release tag.

## Repeat with Saved Inputs

Every run prints and retains its results directory, even after success. Its
`snapshot/` contains checksummed inputs and can be reused on later runs:

```sh
./testdata/native-regression.sh \
  --ref-tag v2.15.2 \
  --snapshot /tmp/sab-regression-EXAMPLE/snapshot
```

The snapshot's year becomes the default when `--year` is omitted. Reusing inputs
avoids upstream fetches, and pinning the tag avoids latest-release discovery.
The release build cache is verified and reused; the working binary is rebuilt.
An old snapshot can intentionally compare code against older captured mappings
and metadata. Delete or stop selecting it when fresh upstream coverage is needed;
cache timestamp renewal is not a claim that the captured content is current.

For an offline comparison with existing binaries:

```sh
./testdata/native-regression.sh \
  --ref-tag v2.15.2 \
  --ref-binary /tmp/released-server \
  --work-binary /tmp/working-server \
  --snapshot /tmp/sab-regression-EXAMPLE/snapshot \
  --output /tmp/sab-regression-new-run
```

That path needs only Python and the supplied binaries, with loopback networking;
no Go, Git, GitHub CLI, or external network is used. Existing output directories
are rejected. Supplied binaries and snapshots are never modified.

| Option | Default / meaning |
| --- | --- |
| `--ref-tag` | Latest stable release; `NATIVE_REF_TAG` also works. A supplied reference binary can identify its tag through `--version`. |
| `--ref-binary` | Executable release binary; otherwise build/cache tagged source. |
| `--work-binary` | Executable working binary; otherwise build the current tree. |
| `--snapshot` | Existing checked snapshot directory; otherwise capture live inputs once. |
| `--year` | Snapshot year, or current calendar year minus one. Historical years from current minus nine through minus one are supported. |
| `--output` | New unique directory under the system temporary directory. An explicit path can place retained artifacts elsewhere. |
| `--cache-dir` | `$TMPDIR/sab-native-binaries`, for verified release builds. |
| `--requests` | 25 warm HTTP samples per season/build; `0` disables sampling. |
| `--timeout` | 360 seconds per build/capture. Replay readiness is capped at 30 seconds; requests and shutdown also have finite deadlines. |

Application settings are controlled by the runner. Personal `PORT`, cache paths,
filters, prewarm years, mapping URL, and admin token do not affect the comparison.
Writable service copies use independent temporary directories and ports.

## Results and Performance

`results.json` records success/failure, year, host, binary versions and SHA-256
hashes, available source commits and dirty state, snapshot identity, phase wall
times, response counts, and warm HTTP timings. Logs, ordered response JSON, and
readable mismatch diffs remain beside the report. Successful runs also retain
this evidence. A failed setup exits nonzero with its artifact location; inspect
capture logs for rate-limit waits rather than treating them as output regressions.

The default warm timing sample measures SUMMER, WINTER, and ALL over local HTTP,
alternating requests between builds after hydration. Median and nearest-rank p95
include HTTP/client overhead; they do not report allocation bytes, production
throughput, or production p95/p99. Timings never determine pass/fail.

For allocation and CPU comparisons, use the Go benchmark harness with identical
fixtures and the same host/toolchain for both versions:

```sh
go test -race -run TestHandleListPipelineMatrix ./cmd/server
go test -run '^$' -bench 'BenchmarkList(Hit|WinterHit)' -benchmem -count=6 ./cmd/server
```

Use controlled AniList unit tests for pagination, 429 backoff, Retry-After bounds,
missing pages, cancellation, and malformed responses. Do not deliberately
exhaust the live upstream rate limit for a local correctness check.
