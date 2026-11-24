# Product Spec: Rootless Deployment

Support conventional Docker, rootless Docker and rootless Podman without silently changing data ownership. A read-only root and non-root process must still allow persistent cache updates and clean shutdown.

[Implementation](TECH.md). Original evidence: [PR #147](https://github.com/calmcacil/sonarr-anime-bridge/pull/147).
