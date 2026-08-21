# Tech Spec: Operator Diagnostics and Runtime Smoke Coverage

## Context

This document translates the behavior in [PRODUCT.md](./PRODUCT.md) into changes against commit [`a0fc26a65503a62bd087d55103d437fbeab02f94`](https://github.com/calmcacil/sonarr-anime-bridge/tree/a0fc26a65503a62bd087d55103d437fbeab02f94).

- [`cmd/server/main.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/cmd/server/main.go) owns startup ordering, runtime-directory validation, HTTP routing, `/health`, and the image healthcheck client. Startup validates data directories before opening SQLite or loading mappings, starts the listener, then prewarms configured years asynchronously.
- [`internal/cache/cache.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/internal/cache/cache.go) owns the one-row-per-year SQLite cache. `HasYearContext` can determine whether a configured prewarm year has any cached data without reading or mutating the row.
- [`internal/scheduler/scheduler.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/internal/scheduler/scheduler.go) owns resolver state, unloaded retry cadence, mapping refresh, prewarm, stale refresh, and fetch logging. `ResolverLoaded` already exposes the lock-free resolver availability signal needed by health.
- [`cmd/server/main_test.go`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/cmd/server/main_test.go) covers current healthy, degraded, and method-rejection health behavior plus runtime-directory validation.
- [`.github/workflows/ci.yml`](https://github.com/calmcacil/sonarr-anime-bridge/blob/a0fc26a65503a62bd087d55103d437fbeab02f94/.github/workflows/ci.yml) builds both supported images and version-smokes the amd64 image, but does not currently start the service against writable or rejected `/data` mounts.

The health extension must preserve the existing aggregate HTTP decisions while adding component detail. Cache readiness is deliberately separate from cache reachability: missing prewarm rows are diagnostic state, not a readiness gate. The existing `/list` behavior is also out of scope: when prior-year `WINTER` cache data is absent, `/list` schedules non-blocking background December backfill and may omit December entries in the current response; stale prior-year cached data remains immediately usable while refresh runs asynchronously.

## Proposed changes

### Health model and handler

1. Add private response types in `cmd/server/main.go` rather than exposing a cross-package health abstraction:

   ```go
   type healthCheck struct {
       Status string `json:"status"`
   }

   type healthChecks struct {
       Cache    healthCheck `json:"cache"`
       Resolver healthCheck `json:"resolver"`
   }

   type healthResponse struct {
       Status string       `json:"status"`
       Reason string       `json:"reason,omitempty"`
       Checks healthChecks `json:"checks"`
   }
   ```

   Named string constants should define the finite values `ok`, `warming`, `degraded`, and `unhealthy`. Keeping these types private limits the contract to JSON rather than creating an internal API other packages must adopt.

2. Pass `cfg.PrewarmYears` into health evaluation. The handler already receives the cache and scheduler; extend its construction to receive the configured years or a copied slice. Do not let the handler retain mutable configuration state.

3. Evaluate cache state in one cache-owned operation. Add `HasYearsContext(ctx context.Context, years []int) (bool, error)` to `internal/cache/cache.go` using one read-only query rather than calling `HasYearContext` once per year. Deduplicate input years before comparison so repeated configuration values cannot make readiness impossible. An empty input has no cached-row requirements but still performs the SQLite reachability probe and propagates any error; a successful probe reports ready.

4. Compute component and aggregate states in this order:
   - Query cache reachability/readiness. A query error yields cache `unhealthy` and aggregate `unhealthy` with HTTP `503`.
   - A successful query yields cache `ok` when every configured year exists and `warming` otherwise.
   - Read `sched.ResolverLoaded()` independently. Loaded yields resolver `ok`; unloaded yields resolver `degraded`.
   - If cache is reachable but the resolver is unloaded, aggregate state remains `degraded` with HTTP `503` and retains the existing `reason: "resolver not loaded"` field.
   - If cache is reachable and the resolver is loaded, aggregate state is `ok` with HTTP `200`, including when cache is `warming`.

   This directly implements PRODUCT invariants 8-17 without making prewarm completion part of the Docker healthcheck.

5. Marshal the typed response through the existing JSON response helpers. Do not include query errors or configuration values in the payload. Continue logging cache health errors server-side.

6. Preserve `runHealthcheck`: it should continue to require HTTP `200` and need not parse the response body. The structured body is for diagnostics; the aggregate HTTP status remains the image health contract.

7. Confirm `HEAD` behavior through the existing server stack. If the handler currently writes a body for direct `HEAD` invocation, ensure the actual HTTP surface suppresses it and add a server-level test rather than duplicating method-specific response construction.

### Operator documentation

8. Add `docs/OPERATIONS.md` implementing PRODUCT invariants 1-7. Link it from the README and keep the README overview brief rather than duplicating the runbook.

9. Document only fields emitted by the current service. Group logs by `type` and explain contextual fields separately. Example commands must use bearer headers, never URL query tokens, and must avoid printing secrets.

10. Document the new component health response as the target contract shipped with this change. Include `ok`, `warming`, `degraded`, and `unhealthy` examples, emphasizing that `warming` remains aggregate `ok`.

### Runtime smoke coverage

11. Add a focused shell script under `testdata/` for the two container scenarios rather than embedding a long state machine in YAML. The script should accept the image tag, use `set -eu`, allocate temporary directories, name containers uniquely, and install a cleanup trap before starting resources.

12. Writable scenario:
   - Create a host directory owned by the selected non-root UID/GID.
   - Provide controlled mapping data or a controlled mapping endpoint so success does not depend on a mutable release download. Reuse an existing valid mapping fixture if available; otherwise add the smallest valid fixture accepted by the loader.
   - Start the image with the host directory mounted at `/data` and a loopback-published random host port.
   - Poll bounded container health/state, printing `docker logs` on failure.
   - Assert HTTP `200`, the complete health JSON shape, and expected files under `/data`.

13. Unwritable scenario:
   - Create an existing directory that the selected runtime UID/GID cannot write while remaining inspectable by the CI user.
   - Start the image with the same runtime identity and mount.
   - Require non-zero exit within a bounded timeout and match the stable runtime-directory validation prefix rather than host-specific permission text.
   - Assert no cache, sidecar, mapping, metadata, or write-probe files were created.

14. Invoke the script from the amd64 image step in `.github/workflows/ci.yml` after the version smoke. Keep the arm64 build-only gate unchanged: GitHub-hosted amd64 runners can exercise runtime behavior once while compilation continues to cover both architectures.

15. Do not add Docker runtime smoke to `make check`; Docker is not guaranteed on every developer workstation. Document the script as the local equivalent when Docker is available.

## Testing and validation

- Extend `cmd/server/main_test.go` with table-driven health cases covering PRODUCT 8-17: cache ready, cache warming, resolver unloaded, cache query failure, combined resolver/cache failure, unsupported methods, safe JSON fields, and server-level `HEAD` body behavior.
- Add cache tests for `HasYearsContext`: all present, partially present, none present, empty input with a successful SQLite reachability probe, empty input with a reachability error, duplicate years, and canceled/error context. These defend PRODUCT 11-14 without changing hit/miss counters or `last_hit`.
- Retain `/list` WINTER behavior coverage: an absent prior-year cache schedules non-blocking background backfill and the immediate response may omit December entries; stale prior-year data is returned immediately while refresh is asynchronous. Do not introduce a synchronous-backfill assertion.
- Run `go test -race ./...` to cover the changed handler and cache query under the repository race gate.
- Run `golangci-lint run ./...`, `go vet ./...`, and `go build ./...` for normal source validation.
- Run `python3 scripts/check-doc-links.py` and manually compare `docs/OPERATIONS.md` against PRODUCT 1-7.
- Run the new container script against the locally built amd64 image. Its writable case proves PRODUCT 18-21; its rejected-mount case proves PRODUCT 22-25. CI owns the authoritative Docker execution.
- Run `make check` where Docker is available to preserve PRODUCT 26-27 and the existing build gates.

## Risks and mitigations

- **Health becomes an accidental readiness gate:** keep `warming` informational and test that it returns aggregate `ok`/HTTP `200`.
- **Health leaks configuration:** expose only finite status values; test serialized field names and absence of years, paths, URLs, and errors.
- **Readiness queries distort cache statistics:** use a dedicated count/existence query rather than `GetYearContext`.
- **Permission smoke passes as root:** set an explicit non-root `--user` and assert the container's configured runtime path behavior.
- **Smoke becomes flaky through upstream access:** provide controlled mapping input and bounded polling; print logs only on failure.
- **Filesystem permission behavior differs under rootless Docker:** assert application exit and stable validation text, not a particular kernel error string.

## Parallelization

Implementation can split into three local agents after this spec is approved:

- **Health agent** — local worktree `/tmp/sab-76-health`, branch `feat/76-health-details`; owns `cmd/server/main.go`, `cmd/server/main_test.go`, `internal/cache/cache.go`, and cache tests.
- **Operations agent** — local worktree `/tmp/sab-76-docs`, branch `docs/76-operations-runbook`; owns `docs/OPERATIONS.md` and README links.
- **Smoke agent** — local worktree `/tmp/sab-76-smoke`, branch `test/76-runtime-smoke`; owns `testdata/` smoke assets and `.github/workflows/ci.yml`.

All three can run concurrently because their file ownership does not overlap. Land them into one issue branch and one PR; after merging the slices, run the complete Go, documentation, and Docker validation centrally. The health JSON schema in PRODUCT 9-15 is the fixed coordination contract consumed by both documentation and smoke coverage.
