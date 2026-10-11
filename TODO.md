# Container Build And Rootless Deployment Work

This tracker replaces the previous TODO content. It covers only the two proposed
spec groups below. Specifications are written; implementation and runtime
acceptance remain pending. Check a task only after its stated evidence exists.

## Specs And Order

| Order | Spec | Scope | Status |
| --- | --- | --- | --- |
| 1 | [Container builds](specs/container-builds/PRODUCT.md) / [TECH](specs/container-builds/TECH.md) | Explicit build inputs, compilation caching, consistent binaries, and local build deduplication. | Implemented in [PR #145](https://github.com/calmcacil/sonarr-anime-bridge/pull/145) |
| 2 | [Rootless deployment](specs/rootless-deployment/PRODUCT.md) / [TECH](specs/rootless-deployment/TECH.md) | Runtime identity, storage, hardening, lifecycle, native rootless tests, and release acceptance. | Pending |

Implement build work first, then runtime integration, then the separate
production follow-up. Keep specs aligned with implementation in each change.
Release Please continues owning versions and changelog; this tracker does not
authorize a release or production activation.

## 1. Container Builds

- [x] Replace the Docker context denylist with an explicit production-input
  allowlist, excluding tests, local files, credentials, databases, and artifacts
  even inside source directories. Replace broad COPY with explicit input copies.
- [x] Add a disposable context-audit harness asserting the full included
  inventory and harmless excluded sentinels; verify excluded-file changes leave
  application compilation cached and real source/dependency changes rebuild.
- [x] Add Go compilation-cache mounts while retaining the module-download layer;
  verify cold correctness and warm/edit reuse on a retained builder.
- [x] Align container/local binary flags with -trimpath and explicit VCS behavior,
  preserving version injection, static linking, notices, and both architectures.
- [x] Refactor local image checks to build amd64 and arm64 once each and reuse
  the native image for its version check; reject unsupported host architectures.
- [x] Integrate context audits and new harness paths into CI/ShellCheck/scope
  checks; keep native candidate reuse, scans, provenance, and digest promotion.
- [x] Record context size and comparable cold/warm/edit build evidence. Document
  GHA cache-mount limits and safe handling of old dirty builder caches.
- [x] Update build/preflight docs and any changed release-input policy; run
  focused regression checks, make docs, and make check.

## 2. Rootless Deployment

- [ ] Prepare the hardened conventional Compose bind recipe with explicit
  identity, create_host_path=false, and unchanged existing storage defaults.
- [ ] Add a rootless Docker Compose example using an explicit named volume and
  the image user; verify initial ownership, writable access, and later reuse.
- [ ] Add a portable rootless Podman Quadlet example using UID/GID 65532:65532
  and keep-id mapping, preserving host-owned bind data without ownership repair.
- [ ] Apply read-only roots, dropped capabilities, no-new-privileges, bounded
  scratch storage where needed, restart behavior, and 30-second container stop
  grace; give the Quadlet supervisor a larger stop timeout.
- [ ] Audit early signal handling, prewarm cancellation, request drain,
  scheduler wait, and cache close. Add controlled cancellation coverage and
  make minimal lifecycle fixes needed to stop within the container grace.
- [ ] Refactor runtime smoke tooling for explicit engines and actual rootless
  assertions. Isolate test storage/sockets/networks and avoid runtime sudo,
  recursive chown, host networking, and production resources.
- [ ] Add controlled loopback mapping fixtures and seeded SQLite/list fixtures
  without weakening URL validation or adding production API overrides.
- [ ] Verify hardening, fixed list output, persistent writes, negative mounts,
  recreation with usable data, degraded upstream outage, unexpected-exit
  restart, explicit stop, and SIGTERM completion without SIGKILL.
- [ ] Provision and validate rootless Docker and Podman on native amd64/arm64 CI.
  Test the same application candidate bytes and make failures block Required.
- [ ] Gate release promotion on both rootless engines testing exact candidate
  platform digests; retain all existing source, scan, and attestation checks.
- [ ] Update runtime/operations/preflight/release docs, AGENTS.md, and affected
  service-baseline specs. Explain rootful versus rootless ownership, health
  versus restart, migration, rollback, and historical-image recovery limits.
- [ ] Run appropriate Go race tests, automation regressions, workflow checks,
  Compose/Quadlet validation, make docs, make check, and the native engine
  matrix; record evidence against the product invariants.

## 3. Production Follow-up In The Media-Stack Repository

- [ ] Prepare a separate reviewed Quadlet change after disposable acceptance:
  non-root keep-id identity, read-only root, and coordinated stop timeouts.
  Preserve appdata ownership, internal networking, environment inventory,
  target membership, and registry auto-updates.
- [ ] Document old/new identity and image references, same-storage migration,
  backup/restore considerations, and rollback; run that repository's source
  validation and CI before requesting production activation.
- [ ] After separately authorized activation, record real host identity,
  writable mounts, health, lifecycle, and data preservation. Disposable tests
  alone do not complete production acceptance.

## Completion

- [ ] Link implementation commits/PRs and validation evidence from both specs.
- [ ] Confirm every required product invariant has evidence; report any blocked
  engine, architecture, release recovery, or production acceptance explicitly.
