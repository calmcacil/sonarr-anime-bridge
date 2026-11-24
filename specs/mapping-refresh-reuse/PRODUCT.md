# Product Spec: Reuse Unchanged Mappings

An unchanged mapping refresh should not rebuild the resolver. Readers keep using a complete snapshot during refresh or upstream failure.

[Implementation](TECH.md). Original evidence: [PR #141](https://github.com/calmcacil/sonarr-anime-bridge/pull/141).
