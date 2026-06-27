# Audit Variance Report

Source blueprint: `.opencode/plans/global_refactor.md`

## VERDICT: FULLY IMPLEMENTED

All previously identified unfinished/missed implementations have been resolved and verified.

## Verification

- `go test ./...`: passed
- `go build ./...`: passed
- `golangci-lint run ./...`: passed
- `go test -race ./...`: passed
- `./testdata/native-regression.sh`: passed

## Resolved Items

- `cmd/server/main.go`: cache miss and winter overflow logs now describe synchronous fetch-before-response behavior.
- `internal/cache/cache.go`: cache DB paths are validated as plain filesystem paths before open/recovery; recovery deletion uses the validated path only.
- `internal/cache/cache_test.go`: busy retry test now uses a deterministic retry hook instead of fixed sleep.
- `internal/mapping/anibridge.go`: mapping URL validation now requires HTTPS by default, allowlists upstream hosts, revalidates redirects, resolves hostnames, rejects unsafe resolved IPs, validates derived metadata sidecar paths, and errors on malformed episode ranges.
- `internal/config/config.go`: runtime path validation accepts the documented `/data` root and temp directories used by regression tests.
- `entrypoint.sh`: recursive `/data` ownership traversal was replaced with targeted ownership updates for known application paths.
- `testdata/native-regression.sh`: warmup wording now reflects synchronous WINTER prior-year fetch behavior.
