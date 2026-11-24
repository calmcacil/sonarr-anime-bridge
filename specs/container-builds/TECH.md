# Tech Spec: Container Builds

The [Dockerfile](../../Dockerfile) copies module manifests separately from source. [.dockerignore](../../.dockerignore) admits production inputs explicitly. BuildKit cache mounts accelerate retained builders; exported layer caches do not guarantee persistence of those mounts. Update release eligibility when adding embedded assets.

[Product contract](PRODUCT.md). Original evidence: [PR #145](https://github.com/calmcacil/sonarr-anime-bridge/pull/145).
