# Product Spec: Native Local Regression

Status: implemented and validated locally. See [TECH.md](TECH.md) for evidence.

## Summary

Developers compare the released server binary with the working server binary
using one captured historical dataset. Local checks and regression comparisons
run without Docker, preserve evidence, and support repeat comparisons offline.

## Behavior

1. `make check` runs the local quality, race, security, workflow, documentation,
   and native build checks without building or starting containers. Container
   validation remains an explicit separate command and CI responsibility.
2. `make regression` and `./testdata/native-regression.sh` run the same native
   comparison. They never create containers or alter the working checkout.
3. The default comparison year is the previous calendar year. Developers can
   select another historical year within the application's supported range.
4. The latest published stable release is the default reference. Developers can
   pin a stable release tag, supply its native binary, supply a working binary,
   select an input snapshot, and select a new results directory. Existing
   `NATIVE_REF_TAG` selection continues to work.
5. When no release binary is supplied, the runner builds that exact tag and
   reuses a verified cached binary on later runs. A working binary is rebuilt
   from the current checkout each run unless explicitly supplied. Binary
   versions, hashes, and available source provenance appear in the results.
6. A supplied release binary must report the selected release version. Invalid
   tags, years, options, binaries, snapshots, or existing output directories
   fail clearly. Failed builds cannot fall back to an older working binary.
7. The first run captures the selected full year and the preceding December
   once through the reference service. Failed or partial fetches are setup
   failures; two empty responses cannot pass as successful regression coverage.
8. Both compared builds consume exactly the same captured year and mapping
   inputs, in separate writable data directories. Neither comparison depends
   on another live AniList or mapping download.
9. A saved snapshot can be reused without fetching upstream data. With supplied
   binaries and a snapshot, comparison requires no Go, Git, GitHub CLI, or
   external network access. Invalid or changed snapshot bytes fail validation.
10. WINTER includes the preceding year's December starts, without requiring
    another full prior year in the saved comparison dataset. An empty preceding
    December is valid; the selected full year must contain show records.
11. All five seasons and both categories are compared as complete ordered JSON
    arrays. IDs, titles, duplicates, and ordering matter; differences fail and
    produce readable artifacts. JSON object property order does not matter.
12. Health and cache readiness, validation errors, supported methods, admin
    authentication, cache clearing, and graceful shutdown are checked for both
    builds. Restarting each build must preserve its cached inputs and responses.
13. The runner controls service settings so inherited application environment
    variables cannot change the dataset, filtering, paths, or authentication.
14. Default timings measure warm SUMMER, WINTER, and ALL HTTP requests with 25
    samples per build. Results include sample count, median and p95 elapsed
    milliseconds and response counts. Developers can disable these samples;
    machine-dependent timing differences never determine correctness.
15. Timing requests use the same inputs and verify response equality. Builds,
    capture, initial hydration, restarts, and cache clearing are outside warm
    measurements. The report distinguishes end-to-end local HTTP timings from
    Go allocation benchmarks and from upstream fetch time.
16. Every run preserves its snapshot, provenance, responses, logs, timings, and
    final success/failure report. The results path is printed at startup and
    exit. Success does not delete evidence or overwrite another run.
17. Readiness, individual requests, capture, builds, and shutdown have finite
    deadlines. Early server exit, timeout, malformed output, or failed shutdown
    fail with useful diagnostics. Cancellation stops every process the run owns.
18. Comparison cleanup only stops processes started by that run. Concurrent
    runs use independent ports and writable directories. User databases and
    existing binaries/snapshots are never modified.

Deterministic Go fixtures, race tests, retry tests, and malformed-data tests
remain the correctness foundation. The native comparison supplements them;
it does not establish production throughput, tail latency, or container behavior.
