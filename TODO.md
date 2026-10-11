# Spec Implementation Tracker

This file tracks implementation, not whether a spec document has been written or
committed. Status reflects the inspected checkout; it does not claim that tests
or live GitHub settings were reverified during a documentation update.

## Release Batch

`decoded-year-cache`, `mapping-refresh-reuse`, `streaming-mapping-parser`, and
`anilist-page-batching` are implemented and validated in PRs #139, #141, #142,
and #144. All local batching correctness/performance criteria and required CI
pass. Leave the generated Release Please PR #140 open with auto-merge disabled
for manual full-batch review before publishing. Feature merges can update that
PR without publishing a version or image.

## Pending Specs

No feature implementations remain pending. Manual release review is tracked
above; documentation follow-ups remain below.

### Completed AniList Validation

- [x] Implement four-page AniList batches with the full existing year dataset,
  ordered assembly, complete-response validation, and bounded retries.
- [x] Keep all attempts below 30 requests/minute per process; share upstream
  cooldowns across concurrent year fetches and preserve cancellation/deadlines.
- [x] Verify exact data/list equivalence, partial-batch failures, cache behavior,
  Winter backfill, and request counts with deterministic tests and snapshot replay.
- [x] Record before/after warm, cold-fetch, concurrent-fetch, and prewarm
  performance; satisfy the [release criteria](specs/anilist-page-batching/TECH.md#performance-release-criteria).
  Fewer requests alone are not proof that the application is faster.
- [x] Run `make check` and required CI, and update the implementation docs and
  tracker with evidence. [PR #144](https://github.com/calmcacil/sonarr-anime-bridge/pull/144)
  passes Required, PR title, CodeQL, quality, race tests, vulnerabilities,
  dependency review, and native amd64/arm64 container smoke/image scans.
  Full-batch release review remains a separate manual step; no version, tag or
  image has been published.

Native local regression improvements are implemented and validated.
Documentation follow-ups are listed separately below.

### Optional Work

- [x] Profile the intermediate resolution result map after decoded reuse.
  It accounts for about 1% of sampled warm allocation bytes; removal is deferred.
  Any later change must preserve duplicate IDs, ordered output, and discovery.

## Implemented Specs

| Status | Spec | Evidence / remaining follow-up |
| --- | --- | --- |
| Implemented | [anilist-page-batching](specs/anilist-page-batching/PRODUCT.md) / [TECH](specs/anilist-page-batching/TECH.md) | Four-page batches, shared compliant pacing/cooldowns and complete-response validation; captured-data equivalence, failure/cache/Winter coverage, race suite, pipeline matrix and native `v2.15.2` replay pass. [Performance evidence](specs/anilist-page-batching/benchmarks/README.md) passes all local criteria across 540 controlled samples; ten-page cold fetch is 6.38 -> 4.70 s at 50 ms latency. [PR #144](https://github.com/calmcacil/sonarr-anime-bridge/pull/144) passes required CI, including both native container platforms. |
| Implemented | [native-regression](specs/native-regression/PRODUCT.md) / [TECH](specs/native-regression/TECH.md) | Container-free local checks; cached/supplied release binaries; historical capture once and checksummed replay; ordered matrix, restart, endpoint, clear and shutdown checks; retained evidence and optional HTTP timings. `make check` and real `v2.15.2` comparison pass. Supplied-binary offline replay takes about 2.1 s; cached-tag replay without timings about 1.5 s on this host. |
| Implemented | [streaming-mapping-parser](specs/streaming-mapping-parser/PRODUCT.md) / [TECH](specs/streaming-mapping-parser/TECH.md) | Streaming skip/extraction, duplicate-key compatibility, differential/fuzz tests, and benchmarks are implemented. Race tests and native release regression pass; [performance evidence](specs/streaming-mapping-parser/TECH.md#implementation-evidence) records about 53% faster parsing, 75.70% fewer allocated JSON bytes, and 55.25% fewer zstd-plus-JSON bytes. [PR #142](https://github.com/calmcacil/sonarr-anime-bridge/pull/142) records passing CI, including native amd64/arm64 builds, version checks, rootless runtime smoke, and image scans. Release PR #140 remains open for manual review. |
| Implemented | [mapping-refresh-reuse](specs/mapping-refresh-reuse/PRODUCT.md) / [TECH](specs/mapping-refresh-reuse/TECH.md) | Source fingerprints, parsed reuse, cache repair, fallback, and serialized publication are implemented. Race tests and native release regression pass; [performance evidence](specs/mapping-refresh-reuse/TECH.md#implementation-evidence) records 98.19% faster unchanged local refresh and 98.12% fewer allocated bytes. [PR #141](https://github.com/calmcacil/sonarr-anime-bridge/pull/141) records CI, including both native container platforms. Release remains deferred with the batch above. |
| Implemented | [decoded-year-cache](specs/decoded-year-cache/PRODUCT.md) / [TECH](specs/decoded-year-cache/TECH.md) | Bounded decoded reuse, coalesced year tasks and batch mapping snapshots are present. Race suite and native release regression pass; [performance evidence](specs/decoded-year-cache/TECH.md#implementation-evidence) records 87% faster canonical warm requests and the 35% bytes/op reduction (below the 50% target). [PR #139](https://github.com/calmcacil/sonarr-anime-bridge/pull/139) records CI, including both native container platforms, and merge status. Release is deferred with the batch above. |
| Implemented | [service-baseline](specs/service-baseline/PRODUCT.md) / [TECH](specs/service-baseline/TECH.md) | Server, cache, scheduler, AniList client, filters, resolver, runtime, and associated tests describe the inspected checkout, including decoded-year reuse, streaming mapping extraction and paced AniList page batches. |
| Implemented | [operator-diagnostics](specs/operator-diagnostics/PRODUCT.md) / [TECH](specs/operator-diagnostics/TECH.md) | [Operator runbook](docs/OPERATIONS.md), structured health checks in the server, and [runtime smoke coverage](testdata/runtime-smoke.sh) are present. |
| Implemented with spec-alignment follow-up | [ci-release-hardening](specs/ci-release-hardening/PRODUCT.md) / [TECH](specs/ci-release-hardening/TECH.md) | Workflow/cache/release-image hardening is present. Later changes refined CI scope and release eligibility; align this older spec with [current policy](docs/CI_RELEASES.md). Live main/release-tag protections are verified below; spec alignment remains. |

### Documentation Follow-ups

- [ ] Align `ci-release-hardening` with current CI events, trusted metadata-only
  PR validation, and release-eligibility checks. Its original full-main-push
  invariant differs from the current intentional policy.
- [x] Confirm live GitHub protections: active [main ruleset](https://github.com/calmcacil/sonarr-anime-bridge/settings/rules/17380910),
  verified 2026-10-11, requires squash PR merges, strict Required/PR title checks,
  resolved review threads and CodeQL; release-tag immutability is also active.
  [Retained ruleset evidence](specs/anilist-page-batching/benchmarks/main-ruleset.json).

## Updating This Tracker

- Mark a spec `In progress` when implementation starts; document partial work.
- Mark it `Implemented` only when required behavior and validation are complete.
  Add the implementation commit or PR and evidence when available.
- Keep optional improvements separate from required acceptance criteria.
- Update the product/technical specs and service-baseline architecture in the
  same change as implementation. Add a row for each new feature spec.
