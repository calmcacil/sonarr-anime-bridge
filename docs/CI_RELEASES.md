# CI and releases

Normal PRs and merge groups receive the `Required` aggregate check. PR titles use Conventional Commits. Trusted release-metadata PRs retain metadata validation and CodeQL; `scripts/ci_scope.py` determines intentional skips. Main pushes do not repeat full PR CI.

## Release setup

Release Please compares production inputs against the highest stable published release before requesting a GitHub App token. Configure repository variable `RELEASE_APP_CLIENT_ID` and secret `RELEASE_APP_PRIVATE_KEY` only for trusted release coordination, never PR workflows. Release Please maintains version metadata and `CHANGELOG.md`; it does not publish images.

## First-release verification

Verify the published GitHub release and exact tag point to the intended main commit. The publisher validates native amd64 and arm64 candidates in rootless Docker and Podman, checks scans and attestations, then promotes by digest. Verify the GHCR full-version index, platform digests, provenance and SBOM. Mutable aliases are not rollback records.

## Failed publication and rollback

The Release Please workflow can be manually re-run from `main` to recover its
release-pull-request coordination. It never publishes an image. To recover a
publication, manually run `Publish release image` from `main` and provide the
existing exact release tag. The publisher still requires the canonical
repository, a stable `vMAJOR.MINOR.PATCH` tag, its existing non-draft published
GitHub Release, an exact tag checkout, and a tag commit already merged into
`main`; it cannot publish a branch, a fork, or an arbitrary reference.

If the immutable full-version image is absent, recovery builds, smoke-tests, and
promotes it. If it already exists, recovery verifies its immutable index digest,
both architecture manifests, source/version labels, and attestations, then
smoke-tests and scans both platform digests before restoring eligible aliases.
For example, recovering `v2.14.4` after `v2.14.5` does not move any aliases;
recovering the highest `v1` release can restore `v1` without moving `latest`
away from `v2`. Any source, platform, digest, registry, or scan failure fails
closed. Registry authentication and network failures are not absence. An existing
full-version tag is never overwritten; issue a patch release to correct a bad
release.

Candidate tags are staging references, not supported installation references.
A failed validation can leave a candidate available for diagnosis, but it cannot
promote that candidate to a public release tag. Recovery starts a fresh candidate
when the exact release tag is absent. Queue overflow or a cancelled run requires
manual recovery. GitHub release immutability applies to newly published releases;
a tag ruleset additionally blocks updates and deletion of historical `v*` tags,
without blocking Release Please from creating new tags.


See the [historical release audit](RELEASE_AUDIT.md), [standards exceptions](standards-exceptions.md) and [deployment rollback](OPERATIONS.md#rootless-migration-and-rollback).
