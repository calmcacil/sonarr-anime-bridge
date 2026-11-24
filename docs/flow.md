# Request lifecycle

1. Validate configuration and data directories, open SQLite, and attempt to load mappings. Start listening before prewarming years.
2. `/list` validates the query and checks resolver readiness. Load the requested year from SQLite; fetch synchronously on a miss. Serve a stale row while scheduling refresh.
3. For winter, merge eligible December starts from the prior year if available, otherwise schedule backfill. A country-restricted request needs country metadata; unrestricted requests can use older rows.
4. Filter the shows, resolve TVDB IDs against one mapping snapshot, and return the Sonarr list. No partial AniList fetch replaces a complete year.
5. On SIGTERM or SIGINT, cancel work, drain HTTP and scheduler workers, then close SQLite.

See [operations](OPERATIONS.md) for failure behavior and [the baseline contract](../specs/service-baseline/PRODUCT.md) for compatibility.
