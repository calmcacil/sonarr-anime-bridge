# Tech Spec: Operator Diagnostics and Runtime Smoke Coverage

Health evaluates SQLite connectivity and resolver readiness independently. An empty reachable cache is warming, not unhealthy. JSON task logs distinguish failed fetches returning an empty fallback from successful empty results. See [operations](../../docs/OPERATIONS.md).

[Product contract](PRODUCT.md).
