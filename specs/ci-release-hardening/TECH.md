# CI and release hardening: implementation

## Context

Inspected baseline: `84313e81384d20f28487515897e117592f680d6d`.

- [CI](https://github.com/calmcacil/sonarr-anime-bridge/blob/84313e81384d20f28487515897e117592f680d6d/.github/workflows/ci.yml)
  runs overlapping container builds and identical setup-go cache profiles.
- [Publisher](https://github.com/calmcacil/sonarr-anime-bridge/blob/84313e81384d20f28487515897e117592f680d6d/.github/workflows/publish.yml)
  labels manual recovery with workflow context and races/downgrades aliases.
- [Makefile](https://github.com/calmcacil/sonarr-anime-bridge/blob/84313e81384d20f28487515897e117592f680d6d/Makefile)
  defaults to the full gate, which CodeQL autobuild selects.
- [Runtime smoke](https://github.com/calmcacil/sonarr-anime-bridge/blob/84313e81384d20f28487515897e117592f680d6d/testdata/runtime-smoke.sh)
  already verifies controlled mapping startup, health, and storage permissions.

## Proposed Changes

- Use a local Go setup composite: setup-go's cache disabled, a shared module
  cache keyed by toolchain/dependencies, and separate revision-refreshing build
  caches for incremental workloads with profile-specific restore prefixes.
  CodeQL Go uses only module caching because extraction requires compilation.
- Share a manual-build CodeQL reusable workflow between CI and Security. Initialize
  Go extraction, run `go build -a ./...` to ensure extraction on warm caches,
  analyze. Keep weekly govulncheck explicit.
  Analyze Python release helpers and Actions definitions in parallel matrix
  entries with build mode `none`; this mode is not supported for Go.
- Use native amd64/arm64 CI runners. Buildx builds/loads one image per platform;
  version, runtime smoke, and Trivy use that image. Scope GHA layer caches per
  workload/platform. Pin multi-platform Docker base digests for repeatability.
- Separate metadata-only `PR title` from `Required`; include `edited` only in the
  metadata workflow. Retain all full-CI event coverage and strict aggregation.
- Move release/version/index/promotion logic into a standard-library Python
  helper with regression tests. Use Buildx's structured manifest/config output,
  not human-output parsing. Accept only explicit manifest-not-found responses
  as absence. Require exactly amd64/arm64 runtime manifests and associated
  attestation descriptors; validate both source revisions and version labels.
- Publisher runs read-only source verification, candidate build, native parallel
  read-only digest validation, then promotion. Checkout automation at
  `github.workflow_sha` separately from `release-source` so older-tag recovery
  can use new helpers. Pass the verified commit/version as explicit OCI labels.
  Candidates use run-scoped staging tags, never public aliases. Promote the
  verified index by digest, without rebuilding, preserving attestations.
- Queue publications repository-wide with `queue: max`, cancel disabled. Read
  all stable published releases for per-track alias selection; inspect current
  aliases to refuse backward or ambiguous moves. Recheck immutable tags before
  each write. Recovery reuses and validates the existing index.
- Scheduled container scans resolve the highest stable published release,
  freeze its index/platform digests, and scan both platform digests directly.
- Add live main ruleset requirements for independent `PR title` and CodeQL
  (`high_or_higher` security, `errors` alerts), preserving other rules. Enable
  immutable releases and tag update/deletion protection without blocking tag
  creation. Record/read back settings; do not merge or publish a release.

## Testing And Validation

- PRODUCT 1-3, 10: actionlint/ShellCheck, automation unit tests including every
  aggregation result and title type, `make check`, real PR CI/CodeQL results and
  cache-save evidence. Compare elapsed CI time with baseline median 337 seconds.
- PRODUCT 4, 6: native CI smoke and Trivy on both platforms; local native smoke;
  disposable-registry integration test with real multi-platform index promotion
  and unchanged digest/attestations. No GHCR publication in tests.
- PRODUCT 5-8: unit tests for stable tag/event/release guards, main-ahead recovery,
  wrong labels, missing/duplicate/foreign platforms, malformed attestations,
  immutable collisions, pagination, numeric ordering, per-track promotions,
  registry errors, and recoverable partial alias publication. Assert workflow
  promotion depends on successful validation and uses verified source labels.
- PRODUCT 9: manually dispatch Security on the branch and verify it scans the
  real release digests without a Docker build; verify explicit govulncheck.
- PRODUCT 11: live API readback of retained main protections, title/CodeQL
  requirements, immutable-release enablement, and tag protections.

## Risks And Rollout

GitHub concurrency queues have a 100-pending-run limit. An overflowing queue
requires manual recovery. Existing immutable images with invalid metadata or
current vulnerabilities fail closed; only a new reviewed release can correct
them. No real release is published during verification. GitHub immutable-release
enablement applies to future releases; the tag ruleset also protects historical
release tags. CodeQL's merge-blocking alerts are a repository setting, not an
inference from workflow success. Cold caches can dominate the first run.
Actionlint v1.7.12 does not yet recognize GitHub's documented `queue` property;
the local gate suppresses only that exact unsupported-key diagnostic and tests
the publisher's fixed queue configuration. Remove the exception when a released
actionlint version supports queueing.

## Parallelization

No worker agents: source trust, digest validation, promotion, and workflow gates
are tightly coupled. Independent local checks and CI matrix jobs run in parallel.
