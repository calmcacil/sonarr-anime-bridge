# Spec Implementation Tracker

This file tracks implementation, not whether a spec document has been written or
committed. Status reflects the inspected checkout; it does not claim that tests
or live GitHub settings were reverified during a documentation update.

## Pending Specs

Recommended implementation order is shown below. The two mapping specs can ship
independently; implementing reuse first makes unchanged-refresh savings easier
to measure separately from changed-file parsing.

| Status | Spec | Purpose | Implementation remaining |
| --- | --- | --- | --- |
| Pending | [decoded-year-cache](specs/decoded-year-cache/PRODUCT.md) / [TECH](specs/decoded-year-cache/TECH.md) | Reduce repeated work in list requests. | Bounded decoded-year reuse, coalesced background year tasks, one mapping snapshot per batch, and validation/benchmarks. |
| Pending | [mapping-refresh-reuse](specs/mapping-refresh-reuse/PRODUCT.md) / [TECH](specs/mapping-refresh-reuse/TECH.md) | Keep parsed mappings when source content is unchanged. | Loader source identity, content checks, reuse/fallback integration, and validation/benchmarks. |
| Pending | [streaming-mapping-parser](specs/streaming-mapping-parser/PRODUCT.md) / [TECH](specs/streaming-mapping-parser/TECH.md) | Parse changed mappings with fewer temporary objects. | Streaming skip/extraction, duplicate-key compatibility, differential tests, and benchmarks. |

### Optional Work

- [ ] Profile removal of the intermediate resolution result map after decoded
  reuse lands. This is optional within `decoded-year-cache`; implement only if
  it measurably helps and preserves duplicate IDs, ordered output, and discovery.

## Implemented Specs

| Status | Spec | Evidence / remaining follow-up |
| --- | --- | --- |
| Implemented | [service-baseline](specs/service-baseline/PRODUCT.md) / [TECH](specs/service-baseline/TECH.md) | Existing server, cache, scheduler, AniList client, filters, resolver, runtime, and associated tests describe the shipped baseline. |
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
