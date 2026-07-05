# Preflight Tests

Agents MUST run these checks before creating any PR. Tests are organized by phase — run them in order.

## Phase 1: Local Quick Checks (every change)

```bash
golangci-lint run ./... && go build ./... && go test -race ./...
```

- **Lint**: golangci-lint (configured in `.golangci.yml`)
- **Build**: `go build ./...` — must compile clean
- **Test**: `go test -race ./...` — all unit tests with race detector

## Phase 2: Behavioral Changes — Native Regression

Run when making changes to filtering, season splitting, winter overflow, resolution, sorting, or the data pipeline.

### Quick one-shot

```bash
./testdata/native-regression.sh
```

Builds candidate (current tree) and reference (latest release tag), runs end-to-end against live AniList, compares tvdbId output sets. Exit zero = pass.

### Manual step-by-step (debugging)

```bash
# Build candidate
go build -ldflags="-s -w" -o /tmp/sab-cand-server ./cmd/server

# Build reference from latest release
LATEST_TAG=$(gh release list --limit 1 --json tagName --jq '.[0].tagName')
git worktree add --detach /tmp/sab-ref-worktree "$LATEST_TAG"
cd /tmp/sab-ref-worktree && go build -ldflags="-s -w" -o /tmp/sab-ref-server ./cmd/server
cd - && git worktree remove /tmp/sab-ref-worktree

# Start both with clean temp dirs
CAND_DATA=$(mktemp -d)
REF_DATA=$(mktemp -d)
PORT=18081 CACHE_DB_PATH="$CAND_DATA/cache.db" MAPPING_PATH="$CAND_DATA/mappings.json.zst" \
  PREWARM_YEARS="$(date +%Y)" DEBUG_ENDPOINTS_ENABLED=true /tmp/sab-cand-server &
PORT=18082 CACHE_DB_PATH="$REF_DATA/cache.db" MAPPING_PATH="$REF_DATA/mappings.json.zst" \
  PREWARM_YEARS="$(date +%Y)" DEBUG_ENDPOINTS_ENABLED=true /tmp/sab-ref-server &

# Wait for both healthy
for i in $(seq 1 90); do
  curl -sf http://localhost:18081/health >/dev/null 2>&1 && \
    curl -sf http://localhost:18082/health >/dev/null 2>&1 && break
  sleep 1
done

# Warmup winter overflow
curl -s "http://localhost:18081/list?season=WINTER&year=$(date +%Y)" > /dev/null
for i in $(seq 1 90); do
  entries=$(curl -sf "http://localhost:18081/cache/stats" | python3 -c \
    "import sys,json;print(json.load(sys.stdin)['entries'])" 2>/dev/null || echo 0)
  [ "$entries" -ge 2 ] && break
  sleep 1
done

# Fetch and compare
for port in 18081 18082; do
  curl -s "http://localhost:$port/list?season=WINTER&year=$(date +%Y)" | jq '[.[].tvdbId] | sort' \
    > /tmp/sab-$port-tvdbids.json
  curl -s "http://localhost:$port/list?season=WINTER&year=$(date +%Y)&category=series-new" | jq '[.[].tvdbId] | sort' \
    > /tmp/sab-$port-tvdbids-new.json
done
diff /tmp/sab-18081-tvdbids.json /tmp/sab-18082-tvdbids.json
diff /tmp/sab-18081-tvdbids-new.json /tmp/sab-18082-tvdbids-new.json

# Shutdown
kill %1 %2; wait 2>/dev/null || true
rm -rf "$CAND_DATA" "$REF_DATA"
```

## Phase 3: Docker Regression (CI-equivalent)

Run for container lifecycle changes, concurrency fixes, or before release.

```bash
# Build images
DOCKER_BUILDKIT=1 docker build \
  --platform=linux/amd64 \
  --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 \
  -t sonarr-anime-bridge:test-amd64 .

DOCKER_BUILDKIT=1 docker build \
  --platform=linux/arm64 \
  --build-arg TARGETOS=linux --build-arg TARGETARCH=arm64 \
  -t sonarr-anime-bridge:test-arm64 .

# Run
DATA_DIR=$(mktemp -d)
chown "$(id -u):$(id -g)" "$DATA_DIR"
docker run -d --name sab-regression \
  --user "$(id -u):$(id -g)" \
  -v "$DATA_DIR":/data \
  -e PREWARM_YEARS="$(date +%Y)" \
  -e DEBUG_ENDPOINTS_ENABLED=true \
  -p 18080:8080 \
  sonarr-anime-bridge:test-arm64

# Wait for health
for i in $(seq 1 90); do
  curl -sf http://localhost:18080/health >/dev/null 2>&1 && break
  sleep 1
done

# Check health
curl -sf http://localhost:18080/health | python3 -m json.tool

# Warmup winter overflow
curl -s "http://localhost:18080/list?season=WINTER&year=$(date +%Y)" > /dev/null
for i in $(seq 1 90); do
  entries=$(curl -sf "http://localhost:18080/cache/stats" | python3 -c \
    "import sys,json;print(json.load(sys.stdin)['entries'])" 2>/dev/null || echo 0)
  [ "$entries" -ge 2 ] && break
  sleep 1
done

# Capture output
curl -s "http://localhost:18080/list?season=WINTER&year=$(date +%Y)" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'series: {len(d)} shows'); [print(f'  {s[\"tvdbId\"]}  {s[\"title\"]}') for s in d[:5]]; print(f'  ... and {len(d)-5} more')"
curl -s "http://localhost:18080/list?season=WINTER&year=$(date +%Y)&category=series-new" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'series-new: {len(d)} shows')"

# Cache stats (should be 2 entries — current + prior year)
curl -s http://localhost:18080/cache/stats | python3 -m json.tool

# Backfill trigger
curl -s "http://localhost:18080/list?season=SPRING&year=$(date +%Y)" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'backfill response: {len(d)} shows')"

# Invalid input
curl -s -w "HTTP %{http_code}\n" "http://localhost:18080/list?season=INVALID&year=2026"

# Graceful shutdown
docker stop sab-regression
docker logs sab-regression 2>&1 | grep -E "shutting down|WARN.*goroutine|error|panic" || echo "Clean shutdown"

# Clean up
docker rm sab-regression
rm -rf "$DATA_DIR"
```

### Baseline comparison against latest release

```bash
# Reference: last released image
REF_DATA=$(mktemp -d)
chown "$(id -u):$(id -g)" "$REF_DATA"
docker run -d --name sab-ref \
  --user "$(id -u):$(id -g)" \
  -v "$REF_DATA":/data \
  -e PREWARM_YEARS="$(date +%Y)" \
  -e DEBUG_ENDPOINTS_ENABLED=true \
  -p 18082:8080 \
  ghcr.io/calmcacil/sonarr-anime-bridge:latest
for i in $(seq 1 90); do
  curl -sf http://localhost:18082/health >/dev/null 2>&1 && break
  sleep 1
done
curl -s "http://localhost:18082/list?season=WINTER&year=$(date +%Y)" | jq '[.[].tvdbId] | sort' > /tmp/sab-ref-tvdbids.json
curl -s "http://localhost:18082/list?season=WINTER&year=$(date +%Y)&category=series-new" | jq '[.[].tvdbId] | sort' > /tmp/sab-ref-tvdbids-new.json
docker stop sab-ref && docker rm sab-ref && rm -rf "$REF_DATA"

# Candidate: test build
CAND_DATA=$(mktemp -d)
chown "$(id -u):$(id -g)" "$CAND_DATA"
docker run -d --name sab-cand \
  --user "$(id -u):$(id -g)" \
  -v "$CAND_DATA":/data \
  -e PREWARM_YEARS="$(date +%Y)" \
  -e DEBUG_ENDPOINTS_ENABLED=true \
  -p 18083:8080 \
  sonarr-anime-bridge:test
for i in $(seq 1 90); do
  curl -sf http://localhost:18083/health >/dev/null 2>&1 && break
  sleep 1
done
curl -s "http://localhost:18083/list?season=WINTER&year=$(date +%Y)" > /dev/null
for i in $(seq 1 90); do
  entries=$(curl -sf "http://localhost:18083/cache/stats" | python3 -c \
    "import sys,json;print(json.load(sys.stdin)['entries'])" 2>/dev/null || echo 0)
  [ "$entries" -ge 2 ] && break
  sleep 1
done
curl -s "http://localhost:18083/list?season=WINTER&year=$(date +%Y)" | jq '[.[].tvdbId] | sort' > /tmp/sab-cand-tvdbids.json
curl -s "http://localhost:18083/list?season=WINTER&year=$(date +%Y)&category=series-new" | jq '[.[].tvdbId] | sort' > /tmp/sab-cand-tvdbids-new.json
docker stop sab-cand && docker rm sab-cand && rm -rf "$CAND_DATA"

# Compare
if diff /tmp/sab-ref-tvdbids.json /tmp/sab-cand-tvdbids.json; then
  echo "series: IDENTICAL to last release"
else
  ADDED=$(comm -13 /tmp/sab-ref-tvdbids.json /tmp/sab-cand-tvdbids.json 2>/dev/null | wc -l)
  REMOVED=$(comm -23 /tmp/sab-ref-tvdbids.json /tmp/sab-cand-tvdbids.json 2>/dev/null | wc -l)
  echo "series: $ADDED added, $REMOVED removed vs last release"
fi
if diff /tmp/sab-ref-tvdbids-new.json /tmp/sab-cand-tvdbids-new.json; then
  echo "series-new: IDENTICAL to last release"
else
  ADDED=$(comm -13 /tmp/sab-ref-tvdbids-new.json /tmp/sab-cand-tvdbids-new.json 2>/dev/null | wc -l)
  REMOVED=$(comm -23 /tmp/sab-ref-tvdbids-new.json /tmp/sab-cand-tvdbids-new.json 2>/dev/null | wc -l)
  echo "series-new: $ADDED added, $REMOVED removed vs last release"
fi
```

Expected minor variations from upstream AniList data changes. Investigate if diffs exceed ±3 tvdbIds for single-season comparisons.

## Phase 4: Integration Tests

For data pipeline changes (schema, resolution, filtering):

```bash
# Generate fresh baselines (deletes existing baseline files)
INTEGRATION=1 INTEGRATION_YEAR=2026 UPDATE_BASELINE=1 \
  go test -run TestIntegration_DataPipeline ./internal/scheduler/ -v

# Run integration tests
# Requires a baseline first; use UPDATE_BASELINE=1 above or ./testdata/generate-baseline.sh 2026.
INTEGRATION=1 go test -run TestIntegration ./... -v
```

## Phase 5: Code Review Validation

For changes flagged by code review (see issue #31):

```bash
# 5a. Verify resolver-not-loaded returns 503 not 200
# Remove mapping file, restart, hit /list, check HTTP status

# 5b. E2E endpoint smoke test
for s in winter spring summer fall; do
  printf "%s: " "$s"
  curl -s "http://localhost:8080/list?season=$s&year=2026" | python3 -c \
    "import json,sys;print(len(json.load(sys.stdin)))"
done
printf "all: "
curl -s "http://localhost:8080/list?season=all&year=2026" | python3 -c \
  "import json,sys;print(len(json.load(sys.stdin)))"
```

> Note: Run existing unit tests with `-race` for concurrency checks; cache miss and inflight behavior use hermetic fake fetchers in unit tests.

## Phase 6: Container Lifecycle

For startup/shutdown/concurrency changes:

```bash
# Cold start — verify synchronous cache miss
docker restart sonarr-anime-bridge
curl -s --retry 30 --retry-delay 1 --retry-connrefused \
  "http://sonarr-anime-bridge:8080/list?season=all&year=2026" | jq length

# Warm restart — verify prewarm skips when cache fresh
docker restart sonarr-anime-bridge
# Should respond in <2s (no re-fetch from AniList)

# Graceful shutdown — no orphaned goroutines
docker stop sonarr-anime-bridge
docker logs sonarr-anime-bridge 2>&1 | grep -E "shutting down|WARN|error|panic" || echo "clean"

# Cache persistence — verify entries survive restart
curl -s http://sonarr-anime-bridge:8080/cache/stats | python3 -m json.tool
```

## Phase 7: AniList Rate Limit Safety

For changes to fetch logic, throttling, or retry:

```bash
# Verify 700ms throttle between requests
# (check AniList client logs or timing around API calls)

# Verify 5s backoff after 429
# (trigger by hitting rate limit, verify throttle resets)

# Verify Retry-After header is respected and clamped
# (mock 429 with Retry-After, verify effective sleep behavior)
```

## Quick Reference

| Phase | Command | When |
|-------|---------|------|
| 1 | `golangci-lint run ./... && go build ./... && go test -race ./...` | Every change |
| 2 | `./testdata/native-regression.sh` | Behavioral changes |
| 3 | Docker regression (see above) | Container/lifecycle changes |
| 4 | `INTEGRATION=1 go test -run TestIntegration ./... -v` | Data pipeline changes |
| 5 | Code review validation tests | Code review findings |
| 6 | Container lifecycle tests | Startup/shutdown changes |
