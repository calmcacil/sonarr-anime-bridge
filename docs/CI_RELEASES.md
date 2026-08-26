# CI and releases

## Local and pull-request validation

Run the CI equivalent from the repository root:

```bash
make check
```

The `CI` workflow runs on every pull request, merge-group candidate, and push to
`main`, and can be run manually. It does not path-filter this small repository.
The stable aggregate check to require in the `main` ruleset is `Required`; enable
it only after a real pull request has reported that exact name successfully.

The `CI` workflow performs dependency review on pull requests, CodeQL analysis,
and a high/critical vulnerability scan of the final container. The separate
`Security` workflow repeats the CodeQL and container scans weekly and on manual
runs. Repository Actions default permissions must remain read-only, and Actions
must not be allowed to approve pull requests.

## Release setup

Release automation is deliberately split into two workflows:

- `Release Please` is the coordination workflow. It runs only for the canonical
  repository's `main` push or a manual run from `main`, uses read-only default
  `GITHUB_TOKEN` permissions, and opens or updates the release pull request. It
  has no GitHub Packages permission and cannot publish a container.
- `Publish release image` is the trusted publisher. Its read-only verification
  job must succeed before the publication job receives `packages: write`; the
  publication job is scoped to the canonical repository and a verified,
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

The manifest records the latest published release. On a trusted push to the
canonical repository's `main`, Release Please opens or updates a release pull
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
image verification. Only then can the separate publication job use its
`packages: write` permission.

The publisher builds Linux `amd64` and `arm64` OCI images with the release
version injected, preserves the non-root `/server` and writable `/data` runtime,
and smoke-tests `--version` plus the running health endpoint. It publishes these
tags:

```text
latest
vMAJOR
vMAJOR.MINOR
vMAJOR.MINOR.PATCH
```

The exact tag and OCI digest are the immutable installation references. OCI
digests are the artifact checksums; do not create separate checksum files. The
publisher emits BuildKit provenance and an SBOM and verifies the remote image
index has exactly one Linux `amd64` and one Linux `arm64` manifest. All mutable
aliases must resolve to the immutable tag's index digest. There is no signing
step because consumers do not have a documented signature-verification path.

## First-release verification

Before enabling release authority, verify a pull request reports `Required`,
then configure the App variable and secret. For the first release:

1. Merge a normal Conventional Commit pull request and confirm Release Please
   opens or updates its release pull request.
2. Confirm the release pull request receives `Required` and review the proposed
   SemVer and changelog.
3. Merge it and verify the tag and non-draft GitHub Release target the reviewed
   `main` commit.
4. Confirm `Publish release image` runs from that published release and reports
   an OCI digest after verifying the two-platform image index and all four tags.
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
publishes it. If it already exists, recovery verifies its immutable index digest,
both architecture manifests, and each manifest's source revision before it
smoke-tests the release image and restores only `latest`, major, and minor
aliases. Any source, platform, or digest mismatch fails closed. An existing
full-version tag is never overwritten; issue a patch release to correct a bad
release.

To roll back, replace the deployment image with a previously verified exact tag
or digest, then pull and recreate the service:

```bash
docker compose pull
docker compose up -d
```

Confirm `/health` succeeds after rollback. Mutable `latest`, major, and minor
tags are convenience selectors and are not rollback records.

## Remaining GitHub settings

After the migration pull request has reported successfully, configure the
repository to allow squash merges only and activate a `main` ruleset requiring
pull requests, resolved conversations, linear history, branch currency, and the
`Required` status check, with zero approvals for the solo maintainer. Block
deletion and force pushes and configure no routine bypass actor. Enable the
dependency graph, Dependabot alerts, secret scanning, push protection, and
private vulnerability reporting where available.
