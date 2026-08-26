# Standards exceptions

This record contains the repository's actual not-applicable decisions and
standard deviations. It records file and process evidence only; it does not
claim that live GitHub settings, rulesets, or repository features have already
been changed.

## Consumer signing contract - N/A

- **Exact rule:** The release standard requires provenance, SBOMs, and signatures
  when the consumer verification contract requires them; signing is not required
  when no consumer can verify it.
- **Reason or missing capability:** The supported artifact is the GHCR OCI
  container, and consumers have no documented signature-verification path.
- **Scope:** All published image tags and digests for this repository.
- **Owner:** Repository maintainers, with the release workflow owner responsible
  for publication integrity.
- **Risk:** Consumers cannot cryptographically verify publisher identity or
  artifact authenticity through a signature.
- **Compensating control:** Retain immutable OCI digests, exact release tags,
  source/tag verification, BuildKit provenance, and an SBOM. Document exact-tag
  and digest rollback rather than treating mutable aliases as rollback records.
- **Review or migration condition:** Reassess before adding a consumer
  verification path or whenever consumers require signatures. Adopt keyless
  signing, publish verification instructions, and update this record before
  claiming signed artifacts.

## Solo review and ownership boundaries - N/A while solo-maintained

- **Exact rule:** The branch-protection standard requires zero approvals for a
  solo-maintainer repository and permits omitting `CODEOWNERS` when no real
  ownership boundary exists. When two maintainers are routinely available, it
  requires one approval from someone other than the author and dismissal of
  stale approvals when new commits are pushed.
- **Reason or missing capability:** Maintenance is currently performed by one
  maintainer, so an independent approval and a path-ownership boundary are not
  available.
- **Scope:** The `main` ruleset's human-review count and repository-wide
  `CODEOWNERS` coverage.
- **Owner:** Repository maintainer.
- **Risk:** No independent human review is available, and future ownership
  boundaries could be missed if the repository grows without reassessment.
- **Compensating control:** Changes use pull requests, the pull request template,
  maintainer review, and the documented always-present `Required` CI contract.
  Actual GitHub enforcement remains a live-setting verification item, not a
  claim made by this file.
- **Review or migration condition:** Reassess whenever maintainership changes.
  Once two maintainers are routinely available, update the live `main` ruleset
  to require one independent approval and dismiss stale reviews on new commits.
  Add team-based `CODEOWNERS` only for real ownership boundaries with a
  routinely available owner.
