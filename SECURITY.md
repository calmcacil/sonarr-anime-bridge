# Security

Report vulnerabilities privately through GitHub's private vulnerability reporting. Do not post credentials or sensitive user data in issues. Rotate exposed credentials before investigating; deleting a secret from Git does not revoke it.

The highest published stable release receives security maintenance. Older images remain available for rollback. Fix vulnerable immutable images with a new release, never by replacing a full-version tag.

Verify the exact image digest, provenance and SBOM. Images are not signed. See the [release runbook](docs/CI_RELEASES.md) and [signing policy](docs/standards-exceptions.md).
