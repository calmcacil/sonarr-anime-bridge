# Security

Report suspected vulnerabilities privately through GitHub's private
vulnerability reporting for this repository. Do not open a public issue with
exploit details, credentials, tokens, private URLs, or user data.

If a credential may have been exposed, revoke or rotate it first, then review
repository, workflow, package, and audit logs. Removing a committed secret from
the latest revision does not make the credential safe.

Maintainers triage dependency, CodeQL, container, secret-scanning, and
govulncheck findings. Release Please is a read-only coordinator that obtains a
short-lived GitHub App token from the repository variable
`RELEASE_APP_CLIENT_ID` and the repository secret
`RELEASE_APP_PRIVATE_KEY`; it has no package-publishing authority. The separate
trusted publisher accepts only a stable tag from an existing, non-draft GitHub
Release in the canonical repository and receives `packages: write` only in its
candidate and promotion jobs. Read-only native validation checks both actual
platform digests before promotion. OCI digests, BuildKit provenance, and SBOMs are
the release integrity contract. Releases are not signed because consumers have no documented
signature-verification path. See `docs/CI_RELEASES.md` for recovery and rollback.

The highest stable published release is security-maintained. Weekly scans cover
its exact GHCR amd64/arm64 artifacts plus current Go dependencies and CodeQL.
Older exact versions are retained for rollback, not automatically rebuilt or
security-maintained. A vulnerable immutable image requires a new reviewed
release; its full-version tag must not be replaced.
