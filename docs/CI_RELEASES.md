# CI and releases

## Local and pull-request validation

Run the CI equivalent from the repository root:

```bash
make check
```

The `CI` workflow runs on every pull request and push to `main`, and can be run
manually. It does not path-filter this small repository. The stable aggregate
check to require in the `main` ruleset is `Required`; enable it only after a real
pull request has reported that exact name successfully.

The separate `Security` workflow performs dependency review on pull requests,
CodeQL analysis, and a high/critical vulnerability scan of the final container.
It also runs weekly. Repository Actions default permissions must remain
read-only, and Actions must not be allowed to approve pull requests.

## Release setup

Release Please needs a GitHub App installed only on repositories that release.
The App needs these repository permissions:

- Contents: read and write
- Pull requests: read and write
- Metadata: read (implicit)

Store its credentials as repository Actions secrets named `RELEASE_APP_CLIENT_ID` and
`RELEASE_APP_PRIVATE_KEY`. The private key is the complete PEM value. Do not put
either value in source, an Actions variable, logs, or pull-request workflows.

The initial manifest records the existing `v2.12.3` release. On a trusted push
to the canonical repository's `main`, Release Please opens or updates a release
pull request. Review its version and changelog and squash merge it manually.
The same non-cancelling workflow then checks out the exact created tag, verifies
the tag source, smoke-tests the versioned image, and publishes a Linux
`amd64`/`arm64` OCI image to GHCR with these tags:

```text
latest
vMAJOR
vMAJOR.MINOR
vMAJOR.MINOR.PATCH
```

The exact tag and OCI digest are the immutable installation references. OCI
digests serve as checksums; the workflow also publishes BuildKit provenance and
an SBOM. There is no separate downloadable archive or signing key, because the
supported artifact is the container and consumers do not have a documented
signature-verification path.

## First-release verification

Before enabling release authority, verify a pull request reports `Required`,
then configure the App and secrets. For the first release:

1. Merge a normal Conventional Commit pull request and confirm Release Please
   opens or updates its release pull request.
2. Confirm the release pull request receives `Required` and review the proposed
   SemVer and changelog.
3. Merge it and verify the Git tag and GitHub Release target the reviewed `main`
   commit.
4. Verify all four image-tag forms resolve to the published digest and the image
   index contains Linux `amd64` and `arm64`.
5. Run `docker run --rm ghcr.io/calmcacil/sonarr-anime-bridge:vX.Y.Z --version`
   and confirm it prints `vX.Y.Z`.
6. Start the exact tag with writable `/data` and confirm its healthcheck passes.

## Failed publication and rollback

If publication fails before the immutable full-version image exists, rerun the
failed `Release` workflow for the original `main` event. Do not move or recreate
the tag. If the full-version image already exists, the workflow verifies its
source revision and restores the mutable aliases without replacing the immutable
version; a mismatched source fails closed. Never overwrite an existing version
tag to repair a bad release—fix the problem and issue a patch release.

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
