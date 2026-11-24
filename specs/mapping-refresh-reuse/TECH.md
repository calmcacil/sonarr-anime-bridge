# Tech Spec: Reuse Unchanged Mappings

The loader tracks the source associated with the published snapshot. Reuse requires evidence the content still matches; a fresh process must load a usable map. Parse replacements before publication and swap the complete resolver atomically.

[Product contract](PRODUCT.md). Original evidence: [PR #141](https://github.com/calmcacil/sonarr-anime-bridge/pull/141).
