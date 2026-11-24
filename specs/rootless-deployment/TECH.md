# Tech Spec: Rootless Deployment

Conventional Compose requires an existing bind. Rootless Docker uses a named volume; Quadlet uses a host-owned bind with keep-id mapping to 65532. Container stop grace is 30 seconds; Quadlet gives systemd 45 seconds. Validate the same candidate under both engines. See [migration and rollback](../../docs/OPERATIONS.md#rootless-migration-and-rollback).

[Product contract](PRODUCT.md). Original evidence: [PR #147](https://github.com/calmcacil/sonarr-anime-bridge/pull/147).
