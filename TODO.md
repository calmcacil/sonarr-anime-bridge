# Spec Implementation Tracker

This file tracks implementation, not whether a spec document has been written or
committed. Status reflects the inspected checkout; it does not claim that tests
or live GitHub settings were reverified during a documentation update.

## Release Batch

Merge the optimization PRs independently, but defer the next release until
`decoded-year-cache`, `mapping-refresh-reuse`, and `streaming-mapping-parser`
are implemented and validated. Leave the generated Release Please PR open with
auto-merge disabled; review and merge it manually when this batch is complete.
Feature merges can update that PR without publishing a version or image.

## Pending Specs

Recommended implementation order is shown below. The two mapping specs can ship
independently; implementing reuse first makes unchanged-refresh savings easier
to measure separately from changed-file parsing.

| Status | Spec | Purpose | Implementation remaining |
| --- | --- | --- | --- |
| In progress | [mapping-refresh-reuse](specs/mapping-refresh-reuse/PRODUCT.md) / [TECH](specs/mapping-refresh-reuse/TECH.md) | Keep parsed mappings when source content is unchanged. | Implementation, race tests, native regression, and benchmarks are complete; final PR/container validation remains. |
| Pending | [streaming-mapping-parser](specs/streaming-mapping-parser/PRODUCT.md) / [TECH](specs/streaming-mapping-parser/TECH.md) | Parse changed mappings with fewer temporary objects. | Streaming skip/extraction, duplicate-key compatibility, differential tests, and benchmarks. |

### Optional Work

- [x] Profile the intermediate resolution result map after decoded reuse.
  It accounts for about 1% of sampled warm allocation bytes; removal is deferred.
  Any later change must preserve duplicate IDs, ordered output, and discovery.

## Implemented Specs

| Status | Spec | Evidence / remaining follow-up |
| --- | --- | --- |
| Implemented | [decoded-year-cache](specs/decoded-year-cache/PRODUCT.md) / [TECH](specs/decoded-year-cache/TECH.md) | Bounded decoded reuse, coalesced year tasks and batch mapping snapshots are present. Race suite and native release regression pass; [performance evidence](specs/decoded-year-cache/TECH.md#implementation-evidence) records 87% faster canonical warm requests and the 35% bytes/op reduction (below the 50% target). [PR #139](https://github.com/calmcacil/sonarr-anime-bridge/pull/139) records CI, including both native container platforms, and merge status. Release is deferred with the batch above. |
| Implemented | [service-baseline](specs/service-baseline/PRODUCT.md) / [TECH](specs/service-baseline/TECH.md) | Server, cache, scheduler, AniList client, filters, resolver, runtime, and associated tests describe the inspected checkout, including decoded-year reuse. |
| Implemented | [operator-diagnostics](specs/operator-diagnostics/PRODUCT.md) / [TECH](specs/operator-diagnostics/TECH.md) | [Operator runbook](docs/OPERATIONS.md), structured health checks in the server, and [runtime smoke coverage](testdata/runtime-smoke.sh) are present. |
| Implemented with spec-alignment follow-up | [ci-release-hardening](specs/ci-release-hardening/PRODUCT.md) / [TECH](specs/ci-release-hardening/TECH.md) | Workflow/cache/release-image hardening is present. Later changes refined CI scope and release eligibility; align this older spec with [current policy](docs/CI_RELEASES.md). Live repository protections are not verified by this tracker. |

### Documentation Follow-ups

- [ ] Align `ci-release-hardening` with current CI events, trusted metadata-only
  PR validation, and release-eligibility checks. Its original full-main-push
  invariant differs from the current intentional policy.
- [ ] Confirm live GitHub protection settings when validating CI/release work;
  their presence cannot be established from source files alone.

## Updating This Tracker

- Mark a spec `In progress` when implementation starts; document partial work.
- Mark it `Implemented` only when required behavior and validation are complete.
  Add the implementation commit or PR and evidence when available.
- Keep optional improvements separate from required acceptance criteria.
- Update the product/technical specs and service-baseline architecture in the
  same change as implementation. Add a row for each new feature spec.
