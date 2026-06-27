# Global Refactor / QA Plan

Scope: full-codebase scan via glob/grep plus focused reads of Go source, shell scripts, Docker, CI, tests, and docs. This is a structural plan only; no code changes are included.

## `.github/workflows/ci.yml`

### Lines 3-13

- **Core problem:** CI ignores `.github/**`, so workflow changes can merge without YAML/pre-commit/build/test validation.
- **Precise modification:** Remove `.github/**` from `paths-ignore`, or add a dedicated workflow-validation job that runs for `.github/**` changes.

### Lines 34-46

- **Core problem:** `pre-commit/action` already runs local Go hooks (`golangci-lint`, `go build`, `go test -race`) from `.pre-commit-config.yaml`; explicit CI steps run the same gates again.
- **Precise modification:** Either keep explicit steps and run pre-commit with `SKIP=golangci-lint,go-build,go-test`, or remove duplicate explicit lint/build/test steps.

## `.github/workflows/publish.yml`

### Lines 29-31, 144-146

- **Core problem:** Tag pushes trigger the workflow, but the `version` job is skipped for tags and `docker` depends on `version`, so tag-triggered Docker builds are skipped.
- **Precise modification:** Let `version` run on tags and output `github.ref_name`, or split tag publishing into a separate job/path that does not need the non-tag version job.

### Lines 96-106

- **Core problem:** Changelog case arm handles `ci`, but the loop never includes `ci`, so CI commits are omitted from generated changelog entries.
- **Precise modification:** Change the loop to include `ci`: `for group in feat fix perf refactor docs test chore ci; do`.

### Lines 130-142

- **Core problem:** The workflow commits `CHANGELOG.md`, then creates a release with `--target "${{ github.sha }}"`, pointing the release to the pre-changelog commit.
- **Precise modification:** After the changelog commit, set the release target to the new `HEAD` SHA, or stop committing changelog changes inside the release job.

### Lines 144-202

- **Core problem:** Docker publishing has no lint/build/test gate in this workflow; direct pushes to `main` can publish untested images because `ci.yml` only runs on PRs.
- **Precise modification:** Add a `test` job running `golangci-lint run ./... && go build ./... && go test -race ./...`, and make `docker` depend on it, or run CI on `push` to `main` and require it before publishing.

## `cmd/server/main.go`

### Lines 70-80

- **Core problem:** `ListenAndServe` errors are logged and swallowed. If bind/listen fails, `run()` continues to prewarm and then blocks waiting for a signal.
- **Precise modification:** Send server startup errors to a buffered `serverErrCh`; after starting the goroutine, `select` between signal receipt and a non-`http.ErrServerClosed` server error, returning the error from `run()`.

### Lines 88-91

- **Core problem:** `signal.Notify` is not paired with `signal.Stop`, creating a minor lifecycle leak in repeated `run()` invocations/tests.
- **Precise modification:** Add `defer signal.Stop(sigCh)` immediately after `signal.Notify(sigCh, ...)`.

### Lines 109-205

- **Core problem:** `/list` accepts all HTTP methods; expensive cache miss/fetch work can be triggered by POST/PUT/etc.
- **Precise modification:** At the top of `handleList`, reject non-`GET`/`HEAD` methods with `405 Method Not Allowed`.

### Lines 125, 219, 222, 225, 258, 264

- **Core problem:** `ResponseWriter.Write` errors are ignored, hiding client disconnects/partial writes.
- **Precise modification:** Make `writeJSON` return `error` and check/log it at call sites; for inline writes, use `if _, err := w.Write(...); err != nil { slog.Warn(...) }`.

### Lines 129-139

- **Core problem:** Invalid provided `year` values (`abc`, `0`, negative) silently fall back to current year, producing misleading responses.
- **Precise modification:** If `yearStr != ""`, return `400` when `strconv.Atoi` fails or `y <= 0`; keep the existing ±10 year range check after successful parse.

### Lines 157, 175, 193

- **Core problem:** `context.WithoutCancel(r.Context())` detaches fetches from client cancellation and server shutdown. Request-triggered upstream fetches/DB writes can continue after disconnect/shutdown.
- **Precise modification:** Use cancellable bounded contexts. For synchronous request work, prefer `r.Context()` plus a timeout shorter than `WriteTimeout`; for background refreshes, use an app-level context that is canceled on shutdown and has per-operation timeout.

### Lines 171-178, 187-195

- **Core problem:** Logs describe “triggering” backfills/refreshes, but both winter overflow and stale refresh are synchronous in the request path and can block responses.
- **Precise modification:** Either make them truly async through a tracked scheduler worker/queue with bounded context and panic recovery, or update logs/docs/tests to state that refresh is synchronous.

### Lines 208-227

- **Core problem:** `/health` accepts all HTTP methods.
- **Precise modification:** Reject methods other than `GET`/`HEAD` with `405`.

### Lines 230-239

- **Core problem:** `/cache/stats` accepts all HTTP methods and ignores future DB stats errors if `Stats` is fixed to return errors.
- **Precise modification:** Require `GET`/`HEAD`; after refactoring `Stats` to return `(CacheStats, error)`, return `500` on stats query failure.

### Lines 230-258

- **Core problem:** `/cache/stats` exposes operational state and `/cache/clear` destructively clears cache without authentication or feature gating.
- **Precise modification:** Add config such as `ADMIN_TOKEN`/`DEBUG_ENDPOINTS_ENABLED`; require `Authorization: Bearer <token>` for debug endpoints or disable them by default.

### Lines 281-290

- **Core problem:** Recovery middleware calls `WriteHeader(500)` even if the handler may have already written headers/body before panicking.
- **Precise modification:** Enhance `statusResponseWriter` to track whether headers were written; in recovery, only send `500` if headers were not already written, otherwise just log the panic.

## `cmd/server/main_test.go`

### Lines 166-191

- **Core problem:** `TestHandleList_CacheMiss` can hit live AniList via `FetchAndStore`, making unit tests network-dependent/flaky; expectation also conflicts with synchronous cache-miss implementation.
- **Precise modification:** Introduce a scheduler fetcher/client interface or test constructor using a fake fetcher; make the test deterministic and align expected response with the chosen cache-miss contract.

### Missing coverage for `main.go` lines 129-139

- **Core problem:** No tests cover non-numeric, zero, or negative `year` values.
- **Precise modification:** Add table tests asserting `400` for `year=abc`, `year=0`, and `year=-1` after fixing validation.

### Missing coverage for `main.go` lines 208-258

- **Core problem:** Method validation and destructive cache clear behavior are under-tested.
- **Precise modification:** Add tests for non-GET `/health`, non-GET `/cache/stats`, `GET /cache/clear` returning `405`, and `POST /cache/clear` clearing entries/resetting stats.

## `internal/anilist/anilist.go`

### Lines 20-26, 328

- **Core problem:** API base URL and HTTP client behavior are hardcoded, preventing hermetic tests for pagination, retries, 429s, GraphQL errors, and malformed JSON.
- **Precise modification:** Add constructor options or `NewWithClient(baseURL string, httpClient *http.Client, sleeper/backoff ...)`; use `httptest.Server` in tests.

### Lines 257-306

- **Core problem:** Pagination has no hard page limit. If upstream/mock returns `hasNextPage=true` forever, `FetchYear` loops until context cancellation while accumulating results.
- **Precise modification:** Add a sane `maxPages` constant/config and return an error when exceeded.

### Lines 317-321, 351-355

- **Core problem:** `time.After` in cancellation selects retains timers until fire when `ctx.Done()` wins; `Retry-After` can be very long.
- **Precise modification:** Replace with `time.NewTimer`, `Stop`, and drain pattern via a helper like `sleepContext(ctx, d) error`; cap retry-after duration.

### Lines 348-355

- **Core problem:** Any positive `Retry-After` is honored, potentially parking goroutines for arbitrary duration.
- **Precise modification:** Clamp `Retry-After` to a configured maximum and log when clamped.

### Lines 362-369

- **Core problem:** Non-200 response bodies are read fully into memory.
- **Precise modification:** Read at most a small limit, e.g. `io.ReadAll(io.LimitReader(resp.Body, 64<<10))`.

### Lines 377-383

- **Core problem:** Successful JSON response decoding has no body size cap.
- **Precise modification:** Wrap `resp.Body` in `io.LimitReader` before `json.NewDecoder` with a documented maximum response size.

## `internal/cache/cache.go`

### Lines 39-57

- **Core problem:** On `SQLITE_BUSY` during open, recovery deletes `path`, `path-wal`, and `path-shm`. If `CACHE_DB_PATH` is misconfigured, this can remove arbitrary writable files.
- **Precise modification:** Validate `CACHE_DB_PATH` before `Open`; constrain recovery deletion to approved paths under `/data` and expected cache file names/extensions.

### Lines 77-79

- **Core problem:** SQLite path is accepted as a raw driver DSN.
- **Precise modification:** Validate `CACHE_DB_PATH` as a plain filesystem path; reject URI/DSN query syntax unless explicitly supported.

### Lines 156-170

- **Core problem:** `execWithRetry` is not context-aware (`Exec` + `time.Sleep`), so shutdown/request cancellation cannot interrupt busy waits/retries.
- **Precise modification:** Change to `execWithRetry(ctx context.Context, ...)`, use `ExecContext`, and replace `time.Sleep` with a cancellable timer/select.

### Lines 177-189

- **Core problem:** `GetYear` treats every `Scan` error as a cache miss. Corruption, closed DB, permission, and transient DB errors are hidden and can trigger unnecessary upstream fetches.
- **Precise modification:** Change signature to `GetYear(ctx, year) (data []byte, fresh bool, ok bool, err error)`; use `QueryRowContext`; return miss only for `errors.Is(err, sql.ErrNoRows)` and propagate other errors.

### Lines 197, 305-306

- **Core problem:** `SetLastHitDebounce` writes `lastHitDebounce` unsynchronized while `GetYear` reads it, creating a data race if used after serving starts.
- **Precise modification:** Make it test-only/unexported and only call before concurrency starts, or store duration in an atomic/int64 or protect with a mutex.

### Lines 228-234

- **Core problem:** `Clear` resets hit/miss counters but leaves `lastHitTimes` and `lastHitFailed` populated, preserving stale debounce/failure state after clearing cache.
- **Precise modification:** Reinitialize or range-delete both `sync.Map` fields after successful `DELETE`.

### Lines 237-240

- **Core problem:** `HasYear` ignores DB errors and returns false, causing DB failures to masquerade as missing years.
- **Precise modification:** Change to `HasYear(ctx, year) (bool, error)` using `QueryRowContext`; propagate errors to callers.

### Lines 243-245, 248-253, 278-285, 293-300

- **Core problem:** DB operations ignore context (`Exec`, `Query`, `Ping`) and cannot be interrupted during shutdown.
- **Precise modification:** Add context-bearing methods or update signatures to use `ExecContext`, `QueryContext`, and `PingContext`.

### Line 289

- **Core problem:** `RowsAffected` error is discarded.
- **Precise modification:** Check and return it: `n, err := result.RowsAffected(); if err != nil { return 0, err }`.

### Lines 293-296

- **Core problem:** `Stats` ignores DB errors and can report zero/incorrect entries on DB failure.
- **Precise modification:** Return `(CacheStats, error)` and update `/cache/stats`, scheduler stats logging, and callers to handle errors.

## `internal/cache/cache_test.go`

### Lines 471-543

- **Core problem:** `TestExecWithRetry_RecoversFromBusy` relies on `time.Sleep(100ms)` and SQLite lock timing, making it flaky under slow CI.
- **Precise modification:** Replace sleep with a deterministic synchronization hook/counter in retry logic or a barrier that observes the blocked write before releasing the lock.

### Lines 555-592

- **Core problem:** `TestConcurrentAccess_NoBusyErrors` is a 50-goroutine SQLite stress test in the normal unit suite; it can be CI-flaky under load.
- **Precise modification:** Move behind a stress/integration flag or reduce to a deterministic concurrency unit test with controlled retry hooks.

## `internal/config/config.go`

### Lines 37, 40-41

- **Core problem:** Security-sensitive sinks (`CACHE_DB_PATH`, `MAPPING_PATH`, `MAPPING_URL`) are loaded without validation.
- **Precise modification:** Validate paths and URL during config load: constrain paths to `/data` by default, reject URI/DSN syntax for DB path, require `https` mapping URL, and allowlist expected host or explicitly opt into custom URLs.

### Lines 88-94

- **Core problem:** Invalid boolean env values silently fall back to default, hiding misconfiguration.
- **Precise modification:** Log a warning when `strconv.ParseBool` fails, including key/raw value and default used.

### Lines 97-103

- **Core problem:** Invalid integer env values silently fall back to default except later port range warnings.
- **Precise modification:** Log a warning when `strconv.Atoi` fails before returning default.

### Lines 125-145

- **Core problem:** `PREWARM_YEARS` accepts any positive year, allowing nonsensical years and unnecessary upstream/cache activity.
- **Precise modification:** Apply the same bounded policy as `/list` (current year ±10) or define explicit min/max; warn and skip out-of-range years.

## `internal/mapping/anibridge.go`

### Line 99

- **Core problem:** Metadata read errors are discarded; corrupt/unreadable sidecar metadata is silently treated as empty.
- **Precise modification:** Capture `metaErr`; log a warning for parse/read failures or return an error if strict metadata integrity is required.

### Lines 91-176, 414-484

- **Core problem:** `LoadOrFetch` accepts `ctx`, but file writes, zstd decompression, and JSON parsing do not check cancellation, so shutdown during large parse/write cannot interrupt work.
- **Precise modification:** Check `ctx.Err()` before/after disk writes and pass `ctx` into parsing; check periodically inside the `for dec.More()` loop.

### Lines 243-251, 279-287

- **Core problem:** `MAPPING_URL` is used directly for outbound HEAD/GET and default client follows redirects. A malicious config can SSRF internal/private targets.
- **Precise modification:** Parse/validate URLs before requests; require `https`; allowlist expected hostnames or reject private/loopback/link-local/reserved IPs after DNS resolution; set `CheckRedirect` to revalidate redirects.

### Lines 263-265, 309-319

- **Core problem:** Malformed upstream MD5 headers are silently ignored, weakening advertised checksum validation.
- **Precise modification:** Log a warning on malformed checksum headers; for `Fetch`, consider returning an error when checksum header exists but cannot be decoded.

### Lines 297-300

- **Core problem:** Full mapping response body is read without a size bound.
- **Precise modification:** Use `io.LimitReader` with a documented compressed mapping maximum (for example 50 MiB plus one byte) and fail if exceeded.

### Lines 325-338, 341-347, 414-484

- **Core problem:** Zstd decompression and JSON parsing are unbounded; malicious/corrupt mapping can consume excessive CPU/memory.
- **Precise modification:** Wrap the zstd reader with a decompressed byte limit before JSON decoding; fail when decoded size exceeds a documented maximum.

### Lines 350-377

- **Core problem:** `MAPPING_PATH` is accepted as an arbitrary write target; `MkdirAll`, temp creation, and rename can write anywhere the process has permission.
- **Precise modification:** Validate and clean `MAPPING_PATH`; constrain to `/data` by default; optionally reject symlinked parent directories.

### Lines 384-385

- **Core problem:** Sidecar metadata path is derived by string append without path validation, compounding arbitrary path issues.
- **Precise modification:** Validate the mapping path first, then derive sidecar path from the validated path.

### Lines 443, 453, 471, 487-491

- **Core problem:** `skipValue` logs decode failures but callers continue parsing after decoder desynchronization, potentially accepting partial/corrupt mappings.
- **Precise modification:** Make `skipValue` return `error`; propagate that error from `parseAnibridgeJSON`.

### Lines 499-503

- **Core problem:** `extractTVDB` silently treats malformed entry JSON as unresolved mapping.
- **Precise modification:** Return `(int, bool, error)` or log the malformed entry with enough context; for mapping parse integrity, propagate errors.

### Lines 542-550

- **Core problem:** Malformed per-entry episode range JSON is silently ignored, which can alter TVDB selection without signal.
- **Precise modification:** Return an error or structured warning from `countSourceEpisodes` and propagate/log the descriptor context.

## `internal/scheduler/scheduler.go`

### Lines 55-64

- **Core problem:** `LoadResolver` always uses `context.Background()`, so startup mapping download/parse cannot be canceled by shutdown/test timeout.
- **Precise modification:** Change `LoadResolver(ctx context.Context)` and pass the application context from `run()`/tests.

### Lines 121-139

- **Core problem:** `Prewarm` does not check `ctx.Err()` between years and silently ignores corrupt fresh cache data by refetching without logging the unmarshal failure.
- **Precise modification:** Check `ctx.Err()` at the top of each loop; log corrupt fresh-cache unmarshal errors with year before refetching.

### Lines 148-168

- **Core problem:** Prior-year winter overflow unmarshal errors are silently ignored, changing results without signal.
- **Precise modification:** Log the unmarshal error with prior year and source; consider returning a processing error if prior-year data exists but is corrupt.

### Lines 193-210

- **Core problem:** First caller’s context owns the shared in-flight fetch. If it cancels, all waiters receive that cancellation even if their contexts are still valid.
- **Precise modification:** Decouple producer fetch context from individual waiters using a scheduler/app-level context plus per-fetch timeout, or switch to `singleflight.Group.DoChan` with app-level fetch context.

### Lines 206-210

- **Core problem:** A panic in `FetchAndStore` closes `done` with `result.err == nil`, so concurrent waiters can incorrectly see success.
- **Precise modification:** In the defer, recover panics, set `err = fmt.Errorf("fetch year %d panic: %v", year, r)`, assign `result.err`, close/delete, and then optionally re-panic after publishing the error.

### Lines 249-261

- **Core problem:** Background stale refresh processes all years serially with no per-year timeout; one slow operation can block prune/stats until it completes.
- **Precise modification:** Use per-year `context.WithTimeout(ctx, 2*time.Minute)` and check `ctx.Err()` between years.

### Lines 264-276, 282-294

- **Core problem:** Prune and VACUUM calls cannot be canceled because cache methods lack context.
- **Precise modification:** After cache context refactor, call `PruneStaleYears(ctx, 14)` and `Vacuum(ctx)`.

### Lines 311-323

- **Core problem:** `Wait` spawns a goroutine per call that can outlive the caller if the wait context expires.
- **Precise modification:** Add a scheduler-owned `done` channel closed once after `wg.Wait()`; have `Wait` select on that channel and `ctx.Done()` without spawning per-call goroutines.

## `internal/scheduler/integration_test.go`

### Lines 30, 45, 78, 97

- **Core problem:** Integration tests use `time.Now().Year()`, causing annual baseline drift and upstream-dependent behavior changes.
- **Precise modification:** Add `INTEGRATION_YEAR` helper with a documented stable default and use it for config and assertions.

### Lines 44, 92

- **Core problem:** Live AniList/mapping work uses `context.Background()`, so hangs can stall tests indefinitely.
- **Precise modification:** Use `context.WithTimeout`, e.g. 2-5 minutes, and pass it through resolver/fetch/prewarm paths after `LoadResolver(ctx)` exists.

### Lines 137-146

- **Core problem:** Integration test writes baseline files automatically when missing, mutating the working tree during tests.
- **Precise modification:** Only write baselines when `UPDATE_BASELINE=1`; otherwise fail with instructions to run the baseline generator.

### Lines 149-164

- **Core problem:** Baseline comparison is exact against live upstream data with no tolerance/approval workflow, producing churn/flakes.
- **Precise modification:** Use deterministic fixtures for CI-like validation or implement a documented tolerance/approval workflow for live integration runs.

## `internal/scheduler/scheduler_test.go`

### Lines 40-45

- **Core problem:** Test uses `time.Sleep(50ms)` to wait for goroutine scheduling.
- **Precise modification:** Replace with a readiness channel/barrier that signals when the waiter is actually blocked on the in-flight result.

### Lines 64-73

- **Core problem:** Unit test calls `FetchAndStore` for 2025, performing real AniList fetches in `go test -race ./...`.
- **Precise modification:** Introduce a fetcher interface/test constructor and use a deterministic fake fetcher.

## `entrypoint.sh`

### Lines 8-13

- **Core problem:** Numeric-only validation allows `PUID=0`/`PGID=0`, running the service as root via `su-exec`.
- **Precise modification:** Reject UID/GID `0` unless an explicit `ALLOW_ROOT=1` escape hatch is set.

### Line 27

- **Core problem:** Recursive `chown -R /data` is expensive on large volumes and can be risky with unexpected mount contents.
- **Precise modification:** Prefer chowning known application paths/files only; if recursive behavior remains, constrain traversal (`find -xdev`, no symlink following) and document expected `/data` contents.

## `Dockerfile`

### Line 18

- **Core problem:** Runtime image installs `wget` only for healthcheck, increasing attack surface.
- **Precise modification:** Add a minimal healthcheck mode to the Go binary or rely on orchestrator external healthchecks; remove `wget` from runtime image if possible.

### Lines 27-28

- **Core problem:** Docker healthcheck hardcodes port `8080`; if `PORT` is changed, container is marked unhealthy despite service listening elsewhere.
- **Precise modification:** Either make in-container port fixed/non-configurable or use a shell healthcheck/wrapper that reads `$PORT`.

## `README.md`

### Lines 30-35, 73-80

- **Core problem:** Documentation says non-prewarmed cache misses trigger async backfill and first response returns `[]`, but implementation does synchronous fetch on miss. README also says prewarm completes before accepting requests, while code starts listener before prewarm.
- **Precise modification:** Decide desired contract. If current code is intended, update README to say listener starts before prewarm and cache misses synchronously fetch then return data/`[]` on failure. If docs are intended, change handler to async backfill on miss.

## `docs/PREFLIGHT_TEST.md`

### Lines 98-103, 149-162

- **Core problem:** Docker/regression checks rely on fixed `sleep 25`, which is flaky with slow networks or large AniList responses.
- **Precise modification:** Replace sleeps with bounded polling of `/health` plus `/cache/stats` expected entries.

### Lines 198-203

- **Core problem:** Docs suggest `PREWARM_YEARS="2026"` controls integration baseline generation, but tests use `time.Now().Year()` directly.
- **Precise modification:** Add `INTEGRATION_YEAR` support to tests and update docs, or remove the misleading `PREWARM_YEARS` example.

### Lines 225-226

- **Core problem:** Notes mention proposed concurrency tests while existing unit tests still use live network paths.
- **Precise modification:** Replace with implemented hermetic tests using fake fetchers for cache miss, in-flight coalescing, cancellation, and error propagation.

## `docs/REGRESSION_TESTS.md`

### Lines 105-109, 156-169

- **Core problem:** Regression procedure uses fixed sleeps for Docker/reference startup.
- **Precise modification:** Replace sleeps with bounded polling for `/health` and required cache entries.

## `testdata/native-regression.sh`

### Lines 100-101

- **Core problem:** Comment says prewarm completes before `ListenAndServe`, but code starts listener before prewarm; `sleep 2` is brittle.
- **Precise modification:** Update the comment and replace fixed sleep with polling for endpoint/cache readiness.

### Lines 211-238

- **Core problem:** Script saves JSON arrays then runs plain `sort` before `comm`, sorting JSON syntax lines rather than numeric TVDB IDs reliably.
- **Precise modification:** Convert arrays to numeric lines before comparison: `jq -r '.[]' file | sort -n` for both reference and candidate.

### Lines 242-250

- **Core problem:** Script reports differences over threshold but exits zero, so automation cannot fail on regressions.
- **Precise modification:** Add `exit 1` in the failure branch when `series_result` or `new_result` is non-zero.
