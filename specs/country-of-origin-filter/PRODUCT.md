# Country of origin filter

Keep unrestricted discovery by default. `DEFAULT_COUNTRIES=JP` selects Japanese productions regardless of format. An explicit `country` list replaces the default; `country=ALL` removes it. Unknown origins do not match a restricted list. Invalid overrides return 400; invalid configured defaults stop startup.

[Implementation](TECH.md).
