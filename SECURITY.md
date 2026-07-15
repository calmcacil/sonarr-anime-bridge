# Security

Report suspected vulnerabilities privately through GitHub's private
vulnerability reporting for this repository. Do not open a public issue with
exploit details, credentials, tokens, private URLs, or user data.

If a credential may have been exposed, revoke or rotate it first, then review
repository, workflow, package, and audit logs. Removing a committed secret from
the latest revision does not make the credential safe.

Maintainers triage dependency, CodeQL, container, secret-scanning, and
govulncheck findings. Release credentials are limited to the shared Release
Please GitHub App and the job-scoped GitHub Packages token described in
`docs/CI_RELEASES.md`.
