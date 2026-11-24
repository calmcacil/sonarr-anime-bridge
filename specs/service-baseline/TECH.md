# Tech Spec: Sonarr Anime Bridge Service Baseline

Listen before prewarming. Cache complete years in SQLite, refresh stale rows in the background and resolve filtered candidates against one mapping snapshot. Shut down by cancelling work, draining requests and closing the cache. See [request lifecycle](../../docs/flow.md) and the [public interface](../../README.md).

[Product contract](PRODUCT.md).
