# Product Spec: Container Builds

Build Linux amd64 and arm64 images from production inputs only. Local files, cached data and fixtures must not enter the build context. A cold build must not depend on previous build caches.

[Implementation](TECH.md). Original evidence: [PR #145](https://github.com/calmcacil/sonarr-anime-bridge/pull/145).
