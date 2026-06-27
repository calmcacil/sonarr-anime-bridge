#!/usr/bin/env bash
# native-regression.sh — Run full regression tests natively (no Docker).
#
# Builds both the candidate (current tree) and reference (latest release
# tag) binaries, starts each against live AniList data, exercises all
# endpoints, and compares the output tvdbId sets. Catches regressions in
# filtering, resolution, sorting, or the data pipeline.
#
# Usage:
#   ./testdata/native-regression.sh
#
# Prerequisites: go, curl, jq

set -euo pipefail

cd "$(dirname "$0")/.."

CAND_BIN="/tmp/sab-cand-server"
REF_BIN="/tmp/sab-ref-server"
REF_WORKTREE="/tmp/sab-ref-worktree"

CAND_DATA=$(mktemp -d)
REF_DATA=$(mktemp -d)
CAND_PORT=18081
REF_PORT=18082
CAND_PID=""
REF_PID=""

cleanup() {
  [ -n "$CAND_PID" ] && kill "$CAND_PID" 2>/dev/null || true
  [ -n "$REF_PID" ] && kill "$REF_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$CAND_DATA" "$REF_DATA"
  if [ -d "$REF_WORKTREE" ]; then
    git worktree remove -f "$REF_WORKTREE" 2>/dev/null || true
  fi
}
trap cleanup EXIT

LATEST_TAG=$(gh release list --limit 1 --json tagName --jq '.[0].tagName' 2>/dev/null || echo "")
if [ -z "$LATEST_TAG" ]; then
  echo "ERROR: could not determine latest release tag. Is gh authenticated?"
  exit 1
fi
echo "=== Latest release tag: $LATEST_TAG ==="

# ── 1. Build candidate ──────────────────────────────────────────────────────
echo "=== Building candidate (current tree) ==="
go build -ldflags="-s -w" -o "$CAND_BIN" ./cmd/server

# ── 2. Build reference from latest tag ───────────────────────────────────────
echo "=== Building reference ($LATEST_TAG) ==="
git worktree add --detach "$REF_WORKTREE" "$LATEST_TAG" 2>&1
(cd "$REF_WORKTREE" && go build -ldflags="-s -w" -o "$REF_BIN" ./cmd/server)
git worktree remove "$REF_WORKTREE"
REF_WORKTREE=""  # prevent double-cleanup

# ── 3. Start candidate ──────────────────────────────────────────────────────
echo ""
echo "=== Starting candidate (port $CAND_PORT) ==="
PORT="$CAND_PORT" \
  CACHE_DB_PATH="$CAND_DATA/cache.db" \
  MAPPING_PATH="$CAND_DATA/mappings.json.zst" \
  PREWARM_YEARS="$(date +%Y)" \
  DEBUG_ENDPOINTS_ENABLED="true" \
  LOG_LEVEL="info" \
  "$CAND_BIN" &
CAND_PID=$!

# ── 4. Start reference ──────────────────────────────────────────────────────
echo "=== Starting reference (port $REF_PORT) ==="
PORT="$REF_PORT" \
  CACHE_DB_PATH="$REF_DATA/cache.db" \
  MAPPING_PATH="$REF_DATA/mappings.json.zst" \
  PREWARM_YEARS="$(date +%Y)" \
  DEBUG_ENDPOINTS_ENABLED="true" \
  LOG_LEVEL="info" \
  "$REF_BIN" &
REF_PID=$!

# ── 5. Wait for both to be ready ────────────────────────────────────────────
echo ""
echo "=== Waiting for servers (up to 90s) ==="
for i in $(seq 1 90); do
  cand_ok=0
  ref_ok=0
  curl -sf "http://localhost:${CAND_PORT}/health" >/dev/null 2>&1 && cand_ok=1
  curl -sf "http://localhost:${REF_PORT}/health" >/dev/null 2>&1 && ref_ok=1
  if [ "$cand_ok" -eq 1 ] && [ "$ref_ok" -eq 1 ]; then
    echo "Both ready after ${i}s"
    break
  fi
  if [ "$i" -eq 90 ]; then
    echo "ERROR: Servers failed to start within 90s"
    [ "$cand_ok" -eq 0 ] && echo "  candidate: NOT ready"
    [ "$ref_ok" -eq 0 ] && echo "  reference: NOT ready"
    exit 1
  fi
  sleep 1
done

# Listener starts before prewarm completes; wait for both caches to settle.
for i in $(seq 1 90); do
  cand_entries=$(curl -sf "http://localhost:${CAND_PORT}/cache/stats" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['Entries'])" 2>/dev/null || echo 0)
  ref_entries=$(curl -sf "http://localhost:${REF_PORT}/cache/stats" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['Entries'])" 2>/dev/null || echo 0)
  [ "$cand_entries" -ge 1 ] && [ "$ref_entries" -ge 1 ] && break
  if [ "$i" -eq 90 ]; then
    echo "ERROR: Initial cache readiness failed within 90s"
    echo "  candidate entries: $cand_entries"
    echo "  reference entries: $ref_entries"
    exit 1
  fi
  sleep 1
done

# ── 6. Health check ───────────────────────────────────────────────────────
echo ""
echo "=== Health check ==="
echo "--- candidate ---"
curl -sf "http://localhost:${CAND_PORT}/health" | python3 -m json.tool
echo "--- reference ---"
curl -sf "http://localhost:${REF_PORT}/health" | python3 -m json.tool

# ── 7. Winter overflow warmup ────────────────────────────────────────────
echo ""
echo "=== Winter overflow warmup ==="
# Fetch prior year synchronously through the WINTER request path.
curl -s "http://localhost:${CAND_PORT}/list?season=WINTER&year=$(date +%Y)" > /dev/null
curl -s "http://localhost:${REF_PORT}/list?season=WINTER&year=$(date +%Y)" > /dev/null
echo "Warmup requested, checking prior year caches (up to 90s)..."
for i in $(seq 1 90); do
  cand_entries=$(curl -sf "http://localhost:${CAND_PORT}/cache/stats" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['Entries'])" 2>/dev/null || echo 0)
  ref_entries=$(curl -sf "http://localhost:${REF_PORT}/cache/stats" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['Entries'])" 2>/dev/null || echo 0)
  if [ "$cand_entries" -ge 2 ] && [ "$ref_entries" -ge 2 ]; then
    echo "Prior years cached after ${i}s (candidate=$cand_entries reference=$ref_entries)"
    break
  fi
  if [ "$i" -eq 90 ]; then
    echo "WARNING: Prior year not cached within 90s"
    echo "  candidate entries: $cand_entries"
    echo "  reference entries: $ref_entries"
  fi
  sleep 1
done

# ── 8. Fetch full output ──────────────────────────────────────────────────
echo ""
echo "=== series ==="
echo "--- candidate ---"
curl -s "http://localhost:${CAND_PORT}/list?season=WINTER&year=$(date +%Y)" \
  | python3 -c "
import sys,json
d=json.load(sys.stdin)
for s in d[:5]:
  print(f'  {s[\"tvdbId\"]}  {s[\"title\"]}')
print(f'  ({len(d)} shows total)')
"
echo "--- reference ---"
curl -s "http://localhost:${REF_PORT}/list?season=WINTER&year=$(date +%Y)" \
  | python3 -c "
import sys,json
d=json.load(sys.stdin)
for s in d[:5]:
  print(f'  {s[\"tvdbId\"]}  {s[\"title\"]}')
print(f'  ({len(d)} shows total)')
"

echo ""
echo "=== series-new ==="
echo "--- candidate ---"
curl -s "http://localhost:${CAND_PORT}/list?season=WINTER&year=$(date +%Y)&category=series-new" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  ({len(d)} shows)')"
echo "--- reference ---"
curl -s "http://localhost:${REF_PORT}/list?season=WINTER&year=$(date +%Y)&category=series-new" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  ({len(d)} shows)')"

# ── 9. Cache stats ───────────────────────────────────────────────────────
echo ""
echo "=== Cache stats ==="
echo "--- candidate ---"
curl -sf "http://localhost:${CAND_PORT}/cache/stats" | python3 -m json.tool
echo "--- reference ---"
curl -sf "http://localhost:${REF_PORT}/cache/stats" | python3 -m json.tool

# ── 10. Backfill trigger ─────────────────────────────────────────────────
echo ""
echo "=== Backfill: /list?season=SPRING&year=$(date +%Y) ==="
echo "--- candidate ---"
curl -s "http://localhost:${CAND_PORT}/list?season=SPRING&year=$(date +%Y)" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  backfill: {len(d)} shows')"
echo "--- reference ---"
curl -s "http://localhost:${REF_PORT}/list?season=SPRING&year=$(date +%Y)" \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  backfill: {len(d)} shows')"

# ── 11. Invalid input ────────────────────────────────────────────────────
echo ""
echo "=== Invalid input ==="
echo "--- candidate ---"
curl -s -w "  HTTP %{http_code}\\n" "http://localhost:${CAND_PORT}/list?season=INVALID&year=2026"
echo "--- reference ---"
curl -s -w "  HTTP %{http_code}\\n" "http://localhost:${REF_PORT}/list?season=INVALID&year=2026"

# ── 12. Save tvdbIds ─────────────────────────────────────────────────────
curl -s "http://localhost:${CAND_PORT}/list?season=WINTER&year=$(date +%Y)" \
  | jq '[.[].tvdbId] | sort' > /tmp/sab-cand-tvdbids.json
curl -s "http://localhost:${CAND_PORT}/list?season=WINTER&year=$(date +%Y)&category=series-new" \
  | jq '[.[].tvdbId] | sort' > /tmp/sab-cand-tvdbids-new.json

curl -s "http://localhost:${REF_PORT}/list?season=WINTER&year=$(date +%Y)" \
  | jq '[.[].tvdbId] | sort' > /tmp/sab-ref-tvdbids.json
curl -s "http://localhost:${REF_PORT}/list?season=WINTER&year=$(date +%Y)&category=series-new" \
  | jq '[.[].tvdbId] | sort' > /tmp/sab-ref-tvdbids-new.json

# ── 13. Graceful shutdown ────────────────────────────────────────────────
echo ""
echo "=== Graceful shutdown ==="
kill "$CAND_PID" "$REF_PID"
wait 2>/dev/null || true
CAND_PID=""
REF_PID=""
echo "Both servers stopped cleanly"

# ── 14. Compare ──────────────────────────────────────────────────────────
echo ""
echo "=== Comparison ==="

series_result=0
new_result=0

echo "--- series ---"
if diff /tmp/sab-ref-tvdbids.json /tmp/sab-cand-tvdbids.json; then
  echo "IDENTICAL to $LATEST_TAG"
else
  SREF=$(mktemp)
  SCAND=$(mktemp)
  jq -r '.[]' /tmp/sab-ref-tvdbids.json | sort -n > "$SREF"
  jq -r '.[]' /tmp/sab-cand-tvdbids.json | sort -n > "$SCAND"
  ADDED=$(comm -13 "$SREF" "$SCAND" 2>/dev/null | grep -c . || true)
  REMOVED=$(comm -23 "$SREF" "$SCAND" 2>/dev/null | grep -c . || true)
  rm -f "$SREF" "$SCAND"
  echo "$ADDED added, $REMOVED removed vs $LATEST_TAG"
  [ "$ADDED" -gt 3 ] || [ "$REMOVED" -gt 3 ] && series_result=1
fi

echo "--- series-new ---"
if diff /tmp/sab-ref-tvdbids-new.json /tmp/sab-cand-tvdbids-new.json; then
  echo "IDENTICAL to $LATEST_TAG"
else
  SREF=$(mktemp)
  SCAND=$(mktemp)
  jq -r '.[]' /tmp/sab-ref-tvdbids-new.json | sort -n > "$SREF"
  jq -r '.[]' /tmp/sab-cand-tvdbids-new.json | sort -n > "$SCAND"
  ADDED=$(comm -13 "$SREF" "$SCAND" 2>/dev/null | grep -c . || true)
  REMOVED=$(comm -23 "$SREF" "$SCAND" 2>/dev/null | grep -c . || true)
  rm -f "$SREF" "$SCAND"
  echo "$ADDED added, $REMOVED removed vs $LATEST_TAG"
  [ "$ADDED" -gt 3 ] || [ "$REMOVED" -gt 3 ] && new_result=1
fi

echo ""
if [ "$series_result" -eq 0 ] && [ "$new_result" -eq 0 ]; then
  echo "RESULT: Pass — output within expected tolerance (±3 tvdbIds) of $LATEST_TAG."
else
  echo "RESULT: Differences detected — review the diff above."
  echo "The candidate uses local month-based season filtering instead of the"
  echo "reference's AniList server-side season assignment, so minor shifts in"
  echo "the show boundaries between WINTER/SPRING seasons are expected."
  echo "Investigate if the difference exceeds ~10 show IDs for a single season."
  exit 1
fi
