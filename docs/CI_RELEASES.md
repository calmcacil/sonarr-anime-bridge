# CI and releases

## Local and pull-request validation

Run the complete local validation from the repository root:

```bash
make check
```

The `CI` workflow runs on every pull request and merge-group candidate, and can
be run manually. It does not use workflow-level path filters: every candidate
receives the stable aggregate check, even when some jobs are intentionally skipped.
The stable aggregate check in the `main` ruleset is `Required`. The independent
`PR title` check validates Conventional Commit titles, including title-only
edits, without repeating full CI or replacing its result. Require both checks
only after a real pull request has reported those exact names successfully.

Normal pull requests receive dependency review, manual-build
CodeQL analysis for Go, Python, and Actions, and native Linux amd64/arm64 container
validation, alongside quality, race-test/coverage, and govulncheck jobs. Each
platform image is built once, then version-tested, runtime-smoked, and scanned with Trivy
for fixable high/critical vulnerabilities. Container builds use separate GHA
layer-cache scopes. Go jobs share dependency downloads but retain separate,
revision-refreshing workload caches. The container matrix supplies both supported
builds, so CI does not repeat them in a standalone cross-build job. `make check`
still cross-builds locally.

| Event | Validation and release work |
|---|---|
| Normal PR | Full CI and `PR title`; registry integration only for release/build automation changes |
| Trusted release-metadata PR | Metadata validation, CodeQL, `Required`, and `PR title`; application/container jobs intentionally skipped |
| Merge-group candidate | Full CI without PR-only dependency review; conditional registry integration |
| Push to `main` | CodeQL default-branch baseline and release eligibility lookup; no repeated full CI |
| Published release | Exact-tag, both-platform image verification, native runtime smoke, scans, attestations, and digest-preserving promotion |
| Manual CI | Full CI without PR-only dependency review, including registry integration |
| Weekly/manual security | CodeQL, govulncheck, and scans of the actual highest stable release image |

`scripts/ci_scope.py` reads the actual Git diff against the event's base commit.
The metadata-only path requires the canonical repository, the
`calm-package-releaser[bot]` author, the same-repository Release Please branch,
and changes to exactly `CHANGELOG.md` and `.release-please-manifest.json`.
The root manifest must increase to a stable SemVer and match the first changelog
release heading. Extra files, human authors, forks, and lookalike branches use
full validation instead. Invalid metadata or unavailable diff history fails.

`Required` checks every expected job and allows only the skips prescribed by
that validated scope. Missing jobs, failed or cancelled jobs, and unexpected
skips fail the aggregate. CodeQL remains mandatory on release-metadata PRs
because the main ruleset requires code-scanning results. Main-branch analysis
keeps the security baseline current; separate concurrency groups prevent it
from cancelling PR or weekly analysis.

The separate `Security` workflow repeats CodeQL and govulncheck weekly and on
manual runs. It resolves the highest stable published version and scans its
actual GHCR platform digests, not a rebuild of main. The current highest stable
release is security-maintained; older exact versions are rollback references.
The main ruleset must also require CodeQL scan results with `high_or_higher`
security alerts and `errors` alerts; a successful upload alone does not imply
that the code has no findings. Repository Actions default permissions remain
read-only, and Actions must not be allowed to approve pull requests.

Actionlint v1.7.12 predates the documented GitHub concurrency `queue` property.
`make workflows` suppresses only that unsupported-key diagnostic and separately
tests the publisher's fixed `queue: max` configuration. Remove this narrow
exception when a released actionlint version supports queueing.

## Release setup

Release automation is deliberately split into two workflows:

- `Release Please` is the coordination workflow. It runs only for the canonical
  repository's `main` push or a manual run from `main`, uses read-only default
  `GITHUB_TOKEN` permissions, and checks release eligibility before obtaining
  an App token or opening/updating the release pull request. It has no GitHub
  Packages permission and cannot publish a container.
- `Publish release image` is the trusted publisher. Its read-only verification
  job must succeed before the candidate/promotion jobs receive `packages: write`;
  those jobs are scoped to the canonical repository and a verified,
  published release tag only.

Release Please uses a GitHub App installed only on repositories that release.
The App needs these repository permissions:

- Contents: read and write
- Pull requests: read and write
- Metadata: read (implicit)

Store the App client ID in the repository Actions variable
`RELEASE_APP_CLIENT_ID` and the complete PEM private key in the repository
Actions secret `RELEASE_APP_PRIVATE_KEY`. The client ID is read through
`vars.RELEASE_APP_CLIENT_ID`; the key is read through
`secrets.RELEASE_APP_PRIVATE_KEY`. Do not put either value in source, logs, or
pull-request workflows.

The manifest records the latest published release. `scripts/release_scope.py`
compares the current Git tree with the highest numeric stable published release,
using version-independent runtime/image inputs: production Go files under
`cmd/` and `internal/`, `go.mod`, `go.sum`, `Dockerfile`, `.dockerignore`, `LICENSE`,
and `NOTICE`. The historical `entrypoint.sh` is also included. File contents,
additions, deletions, and modes count; test files and `internal/testutil/` do not.
Documentation, Actions, test, and release-metadata-only changes do not create a
version when those inputs are unchanged. Net reverts back to the published
inputs also do not create a version. First-release bootstrap is eligible.
Inventory/API errors, missing tags, and an unrecognizable build layout fail
closed. A manual coordinator run does not override eligibility.

Pending application/image changes since the last release remain eligible even
if the latest merge only changes documentation. The comparison is conservative
source-input equality, not binary or semantic equivalence: production comments
or formatting can count as changes. The current application has no embedded
assets; update the input policy if new embedded assets or Docker build inputs
are introduced. Base-image/toolchain refreshes should change their pinned
Dockerfile references rather than rebuild identical inputs under a new version.

On an eligible trusted push to the canonical repository's `main`, Release Please
opens or updates a release pull
request that consolidates every supported Conventional Commit type since the
previous tag into the changelog and resulting GitHub Release description.
Review its version and changelog and squash merge it manually. Documentation,
test, CI, build, refactor, style, revert, and chore entries are retained in the
release record. This configuration controls note completeness; reviewers must
still verify the proposed version against the Conventional Commit intent.
When Release Please publishes the resulting GitHub Release, its read-only
verifier checks that the event is for an exact, stable `vMAJOR.MINOR.PATCH` tag
and a non-draft, non-prerelease release. It checks out that tag with full history
and no persisted credentials, verifies the tag is `HEAD`, and verifies that
commit is an ancestor of `origin/main` before logging in to GHCR for read-only
image verification. Only then can the separate candidate job use its
`packages: write` permission. Recovery checks out trusted automation separately
from the old release source; the OCI revision label always uses the verified
tag commit, never the workflow's main-branch SHA.

The publisher builds Linux `amd64` and `arm64` OCI images with the release
version injected, preserves the non-root `/server` and writable `/data` runtime,
and smoke-tests `--version` plus the running health endpoint. A new image is
built once under a run-scoped `candidate-RUN_ID-ATTEMPT` staging tag. Read-only
native validation jobs verify and smoke-test both actual platform digests and
scan them with Trivy. Only after both pass does promotion copy the unchanged
index digest to these public tags:

```text
latest
vMAJOR
vMAJOR.MINOR
vMAJOR.MINOR.PATCH
```

The exact tag and OCI digest are the immutable installation references. OCI
digests are the artifact checksums; do not create separate checksum files. The
publisher emits BuildKit provenance and an SBOM, requires both for each runtime
manifest, and verifies exactly one Linux `amd64` and one Linux `arm64` manifest.
Promotion preserves the index digest and its attestations. Mutable aliases
advance only when this is the highest published stable version in their track:
`latest` globally, `vMAJOR` within that major, and `vMAJOR.MINOR` within that minor.
Already-newer aliases are preserved even if the release inventory changes.
Publications share one concurrency group with `queue: max` and cancellation
disabled; up to 100 pending runs wait instead of replacing one another. There is
no signing step because consumers do not have a documented signature-verification
path.

## First-release verification

Before enabling release authority, verify a pull request reports `Required`,
then configure the App variable and secret. For the first release:

1. Merge a Conventional Commit pull request with application/image changes and confirm Release Please
   opens or updates its release pull request.
2. Confirm the release pull request receives `Required` and review the proposed
   SemVer and changelog.
3. Merge it and verify the tag and non-draft GitHub Release target the reviewed
   `main` commit.
4. Confirm `Publish release image` runs from that published release and reports
   an OCI digest after validating both platforms and promoting eligible tags.
5. Run `docker run --rm ghcr.io/calmcacil/sonarr-anime-bridge:vX.Y.Z --version`
   and confirm it prints `vX.Y.Z`.
6. Start the exact tag with writable `/data` and confirm its healthcheck passes.
7. Record the exact tag and OCI digest used by the deployment; confirm its
   provenance and SBOM are available from GHCR.

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

To exercise registry promotion without publishing a release:

```bash
sh testdata/release-registry.sh
```

This creates a disposable loopback registry and Buildx builder, tests real
multi-platform index/attestation preservation and monotonic recovery, then removes
its own containers and temporary files. CI runs it in the amd64 container job
when workflows, composite actions, build/release configuration, publication
policy, or smoke/registry fixtures change, and on every manual full-CI run.
Ordinary application changes still receive both native runtime smokes and image
scans, without repeating the registry-promotion fixture.

To roll back, replace the deployment image with a previously verified exact tag
or digest, then pull and recreate the service:

```bash
docker compose pull
docker compose up -d
```

Confirm `/health` succeeds after rollback. Mutable `latest`, major, and minor
tags are convenience selectors and are not rollback records.

## Historical release audit

The [2026-09-30 audit](RELEASE_AUDIT.md) compares all 58 published versions and
inspects public GHCR tags and the source-unchanged candidates. Six versions have
unchanged runtime/image inputs, but their images are not proven byte-identical.
Historical releases, tags, images, and aliases are preserved; the eligibility
gate prevents future automation-only version churn without breaking pinned
installations or rollback references.

## Remaining GitHub settings

After the migration pull request has reported successfully, configure the
repository to allow squash merges only and activate a `main` ruleset requiring
pull requests, resolved conversations, linear history, branch currency, and the
`Required` and `PR title` status checks and CodeQL results, with zero approvals
for the solo maintainer. Enable immutable releases and a release-tag ruleset
blocking tag updates and deletion, with no routine bypass. Block
deletion and force pushes and configure no routine bypass actor. Enable the
dependency graph, Dependabot alerts, secret scanning, push protection, and
private vulnerability reporting where available.
