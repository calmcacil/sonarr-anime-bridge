# Regression Tests

Run these before behavioral changes to confirm no unintended regressions.

## Native Regression (no Docker needed)

Builds candidate (current tree) and reference (latest release tag) binaries,
runs end-to-end against live AniList, compares output.

### Quick one-shot

```bash
./testdata/native-regression.sh
```

Exit code zero = pass.

### Manual step-by-step

```bash
# 1. Build candidate
go build -ldflags="-s -w" -o /tmp/sab-cand-server ./cmd/server

# 2. Build reference from latest release tag
LATEST_TAG=$(gh release list --limit 1 --json tagName --jq '.[0].tagName')
git worktree add --detach /tmp/sab-ref-worktree "$LATEST_TAG"
cd /tmp/sab-ref-worktree
go build -ldflags="-s -w" -o /tmp/sab-ref-server ./cmd/server
cd - && git worktree remove /tmp/sab-ref-worktree

# 3. Start candidate
CAND_DATA=$(mktemp -d)
PORT=18081 CACHE_DB_PATH="$CAND_DATA/cache.db" \
  MAPPING_PATH="$CAND_DATA/mappings.json.zst" \
  PREWARM_YEARS="$(date +%Y)" \
  DEBUG_ENDPOINTS_ENABLED=true \
  /tmp/sab-cand-server &

# 4. Start reference
REF_DATA=$(mktemp -d)
PORT=18082 CACHE_DB_PATH="$REF_DATA/cache.db" \
  MAPPING_PATH="$REF_DATA/mappings.json.zst" \
  PREWARM_YEARS="$(date +%Y)" \
  DEBUG_ENDPOINTS_ENABLED=true \
  /tmp/sab-ref-server &

# 5. Wait for both to be healthy
for i in $(seq 1 90); do
  curl -sf http://localhost:18081/health >/dev/null 2>&1 && \
    curl -sf http://localhost:18082/health >/dev/null 2>&1 && break
  sleep 1
done

# 6. Warmup winter overflow
curl -s "http://localhost:18081/list?season=WINTER&year=$(date +%Y)" > /dev/null
for i in $(seq 1 90); do
  entries=$(curl -sf "http://localhost:18081/cache/stats" | python3 -c \
    "import sys,json;print(json.load(sys.stdin)['entries'])" 2>/dev/null || echo 0)
  [ "$entries" -ge 2 ] && break
  sleep 1
done

# 7. Fetch and compare
for port in 18081 18082; do
  curl -s "http://localhost:$port/list?season=WINTER&year=$(date +%Y)" \
    | jq '[.[].tvdbId] | sort' > /tmp/sab-$port-tvdbids.json
  curl -s "http://localhost:$port/list?season=WINTER&year=$(date +%Y)&category=series-new" \
    | jq '[.[].tvdbId] | sort' > /tmp/sab-$port-tvdbids-new.json
done
diff /tmp/sab-18081-tvdbids.json /tmp/sab-18082-tvdbids.json
diff /tmp/sab-18081-tvdbids-new.json /tmp/sab-18082-tvdbids-new.json

# 8. Shutdown
kill %1 %2; wait 2>/dev/null || true
rm -rf "$CAND_DATA" "$REF_DATA"
```

### Data baseline

Both binaries built from source — no external artifacts needed.
The script uses `gh release list` to find the latest release tag.

## Docker Regression Tests

For container lifecycle, concurrency, and full production data tests.

### Procedure

```bash
# 1. Build test image
DOCKER_BUILDKIT=1 docker build \
  --build-arg BUILDPLATFORM=linux/arm64 \
  --build-arg TARGETOS=linux \
  --build-arg TARGETARCH=arm64 \
  -t sonarr-anime-bridge:test .

# 2. Run with default config
DATA_DIR=$(mktemp -d)
docker run -d --name sab-regression \
  -v "$DATA_DIR":/data \
  -e PUID="$(id -u)" \
  -e PGID="$(id -g)" \
  -e PREWARM_YEARS="$(date +%Y)" \
  -e DEBUG_ENDPOINTS_ENABLED=true \
  -p 18080:8080 \
  sonarr-anime-bridge:test

# 3. Wait for health
for i in $(seq 1 90); do
  curl -sf http://localhost:18080/health >/dev/null 2>&1 && break
  sleep 1
done

# 4. Health check
curl -sf http://localhost:18080/health | python3 -m json.tool

# 5. Warmup winter overflow
curl -s "http://localhost:18080/list?season=WINTER&year=$(date +%Y)" > /dev/null
for i in $(seq 1 90); do
  entries=$(curl -sf "http://localhost:18080/cache/stats" | python3 -c \
    "import sys,json;print(json.load(sys.stdin)['entries'])" 2>/dev/null || echo 0)
  [ "$entries" -ge 2 ] && break
  sleep 1
done

# 6. Capture output
curl -s "http://localhost:18080/list?season=WINTER&year=$(date +%Y)" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'series: {len(d)} shows'); [print(f'  {s[\"tvdbId\"]}  {s[\"title\"]}') for s in d[:5]]; print(f'  ... and {len(d)-5} more')"
curl -s "http://localhost:18080/list?season=WINTER&year=$(date +%Y)&category=series-new" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'series-new: {len(d)} shows')"

# 7. Cache stats (expect 2 entries — current + prior year)
curl -s http://localhost:18080/cache/stats | python3 -m json.tool

# 8. Non-prewarmed endpoint (backfill trigger)
curl -s "http://localhost:18080/list?season=SPRING&year=$(date +%Y)" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'backfill response: {len(d)} shows')"

# 9. Invalid input
curl -s -w "HTTP %{http_code}\\n" "http://localhost:18080/list?season=INVALID&year=2026"

# 10. Graceful shutdown
docker stop sab-regression
docker logs sab-regression 2>&1 | grep -E "shutting down|WARN.*goroutine|error|panic" || echo "No warnings — clean shutdown"

# 11. Clean up
docker rm sab-regression
rm -rf "$DATA_DIR"
```

### Baseline comparison against latest release

```bash
# Reference: last released image
REF_DATA=$(mktemp -d)
docker run -d --name sab-ref \
  -v "$REF_DATA":/data \
  -e PUID="$(id -u)" -e PGID="$(id -g)" \
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
docker run -d --name sab-cand \
  -v "$CAND_DATA":/data \
  -e PUID="$(id -u)" -e PGID="$(id -g)" \
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

Minor differences expected from upstream AniList data churn.
Investigate if diffs exceed ~10 tvdbIds.
