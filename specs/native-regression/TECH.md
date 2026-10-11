# Tech Spec: Native Local Regression

Status: implemented and validated locally against `v2.15.2`.

## Context

This implements [PRODUCT.md](PRODUCT.md). Research baseline is
`95e5e6a5233f18bd2505808385682eb2dd33b0e0`:

- [`testdata/native-regression.sh`](https://github.com/calmcacil/sonarr-anime-bridge/blob/95e5e6a5233f18bd2505808385682eb2dd33b0e0/testdata/native-regression.sh)
  builds both versions, mutates Git worktree/tag state, fetches live data twice,
  waits for row counts rather than exact valid inputs, and discards successful
  evidence. Current-year selection and two clients amplify rate-limit delays.
- [`Makefile`](https://github.com/calmcacil/sonarr-anime-bridge/blob/95e5e6a5233f18bd2505808385682eb2dd33b0e0/Makefile)
  makes Docker a prerequisite of local `check`.
- [`internal/cache/cache.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/95e5e6a5233f18bd2505808385682eb2dd33b0e0/internal/cache/cache.go#L153)
  persists raw year arrays in `year_cache`; prior years are fresh for seven days.
- [`internal/mappingurl/allowlist.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/95e5e6a5233f18bd2505808385682eb2dd33b0e0/internal/mappingurl/allowlist.go)
  already supports explicit insecure loopback mapping URLs for testing.

Published `v2.15.2` has no native binary release assets. Adding binary publication
is outside this change; cache builds from tagged source and accept supplied
binaries instead. Application production code and release workflows need no
behavior changes.

## Proposed Changes

### Entry Points and Build Provenance

Implement `scripts/native_regression.py` using Python's standard library, matching
the existing Python tooling. Keep the shell entrypoint as a small argument/env
forwarding wrapper. Add `make regression`; remove `docker-build` from `check`
and expose an explicit `check-images` target. CI container jobs remain intact.

Use `--ref-tag`, `--ref-binary`, `--work-binary`, `--snapshot`, `--year`,
`--output`, `--cache-dir`, `--requests`, and `--timeout`. Preserve
`NATIVE_REF_TAG`. Resolve a supplied reference binary's version when no tag is
specified. Default tag resolution uses `gh release view` and rejects nonstable
versions. Historical years span current year minus nine through minus one,
leaving the preceding year within the application's prewarm range.

Resolve local tag source or fetch into an isolated temporary Git repository;
export through `git archive`, never checkout/reset the user's worktree. Cache
release builds by commit, host/toolchain, and fixed build flags. Validate cached
binary hash/version before reuse and replace invalid entries only after a
successful build. Build the work binary from the actual checkout with
`-trimpath`, `-buildvcs=false`, `-s -w`, and an explicit version. Record source
commit and dirty status separately; do not imply a dirty build equals HEAD.

### Capture Once, Replay Twice

Capture uses the reference service with explicit production-compatible settings
and configured prewarm years `Y,Y-1`. Readiness requires both exact years to have
valid arrays, not just `.entries >= 2`. Poll within a capture deadline, detect
process exits, then stop successfully before reading SQLite/mapping inputs.
Persist the complete `Y` array, only `Y-1` records starting in December,
compressed mapping bytes, and a versioned JSON manifest with year, capture time,
counts and SHA-256 digests. Capture failures preserve logs and fail the run.

Snapshot validation checks manifest version, selected year, exact expected file
names, digests, JSON array/show shape, nonempty full year, and prior December
membership. SQLite databases are recreated per build with fresh timestamps;
snapshots themselves remain immutable. This refreshes test-cache bookkeeping,
not upstream content, and prevents stale refreshes during an offline replay.

Serve captured mapping bytes with a loopback `ThreadingHTTPServer`, consistent
ETag and HEAD/GET behavior. Give each native process the same URL and mapping
bytes, plus a matching metadata sidecar. Existing
`ALLOW_INSECURE_MAPPING_URL=1` enables this without new application flags.
Both expected cache rows are seeded before startup. Replay must not log any
upstream year fetch; cache clearing is the final operation before shutdown.

### Comparison and Lifecycle

The top-level flow is resolve/build -> capture/load -> seed -> compare endpoints
and ordered matrix -> measure warm requests -> restart/compare -> clear/stop ->
report. Put process/request/snapshot mechanics in narrow helpers; avoid a
framework or new application service layer.

Use distinct temporary service directories and ephemeral ports, explicit
environment settings, and a loopback HTTP client that ignores proxy variables.
Track only owned child processes. Use bounded normal shutdown, kill/reap on
failure or cancellation, and preserve failed exit status. Readiness requires
component health and expected cache rows. Restart tests reuse each build's own
database and compare the same ordered matrix again. A per-run random admin token
keeps diagnostic operations isolated even if another runner wins a port race.

Compare decoded JSON objects without sorting arrays; reject malformed show
outputs and vacuous ALL/series lists. Save response bodies and readable diffs.
Measure SUMMER/WINTER/ALL with `perf_counter`, alternating build requests after
hydration; report median, nearest-rank p95, count and response size. Sampling
does not impose a performance threshold. Retain `results.json` even on errors,
and preserve all artifacts on success. Record phase wall times separately.

Update `docs/PREFLIGHT_TEST.md`, README, AGENTS, TODO, and service-baseline
validation guidance. Remove obsolete manual/container local regression recipes;
link explicit image testing to the existing CI/release documentation.

## Testing and Validation

- PRODUCT 1-6: test Make target separation, argument/year/tag validation, supplied
  binary version checks, cached build reuse/tamper recovery, and build failure.
- PRODUCT 7-10: test exact-year capture readiness, snapshot hashes, nonempty year,
  December extraction, corrupted/missing inputs, fresh seeding, isolated data,
  and loopback mapping HEAD/GET responses.
- PRODUCT 11-13: process-level regression tests use small fake native executables
  and saved inputs to exercise the real runner. Assert changed ordering/titles,
  empty/malformed responses, endpoint failures, restart persistence, cache clear,
  controlled environment, and early exit fail instead of passing silently.
- PRODUCT 14-18: test metrics without timing thresholds, timeout/cancellation
  cleanup, retained success/failure reports, supplied-binary offline replay, and
  refusal to overwrite an existing output directory.
- Run Python tests and shellcheck, then `make check` with writable tool caches.
  Run one real historical capture/comparison against `v2.15.2`; replay that
  snapshot with supplied binaries to verify no external inputs are needed.
  Do not run container checks for this local regression feature.

## Parallelization

Use one implementation agent. Snapshot ownership, native process lifecycle,
failure reporting, and the black-box runner tests share one orchestration flow;
delegation would add coordination without useful independent implementation.

## Implementation Evidence

Validated on 2026-10-11, Linux ARM64, Go 1.27.2, Python 3.13.5:

- `make check` passes without Docker: formatting, module verification/tidy/vet,
  lint (zero issues), all race packages, native amd64/arm64 builds, vulnerability
  scan (none found), workflow/shell checks, Python tooling tests and doc links.
  Follow-up Python checks cover successful early exit before requested shutdown.
- Real `v2.15.2` tag build versus the working checkout passes all ordered season
  and category outputs, endpoints, restart persistence, clear, and shutdown.
  The first capture fetches 482 shows for 2025 and 468 for 2024, retaining only
  16 preceding-December shows alongside the complete 2025 array. Mapping bytes
  and both year inputs have recorded SHA-256 digests.
- First run takes 19.55 seconds: builds 1.27 s, input capture 14.31 s, comparison
  3.97 s. This validation ran beside local checks, so its warm latency samples
  are not performance evidence.
- Real offline replay with supplied binaries and a PATH without development
  tools passes in 2.12 s, including 25 samples per warm season/build. Input copy
  takes 0.008 s; there is no upstream fetch in any replay process log.
- Real verified cached-tag reuse with supplied work binary and saved inputs
  passes in 1.46 s with timing disabled; binary selection takes 0.058 s and
  inputs 0.008 s. These are observations, not machine-dependent test thresholds.

Local artifacts are retained at `/tmp/sab-native-first-validation`,
`/tmp/sab-native-offline-validation`, and `/tmp/sab-native-cache-validation`.
They are run evidence on this host, not required checked-in fixtures.
