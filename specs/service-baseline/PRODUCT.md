# Product Spec: Sonarr Anime Bridge Service Baseline

Return seasonal Sonarr lists from cached AniList years and TVDB mappings. Preserve query validation, winter carryover, per-list country overrides and empty-list fallback on a failed primary fetch. An unavailable resolver returns 503, not an empty success.

[Implementation](TECH.md).
