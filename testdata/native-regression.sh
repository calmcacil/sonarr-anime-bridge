#!/usr/bin/env bash
# Compare complete ordered responses against the latest release using live data.
# Prerequisites: go, gh (authenticated), curl, jq, python3.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d "${TMPDIR:-/tmp}/sab-regression.XXXXXX")
REF_WORKTREE="$WORK/reference"
CAND_PID=""
REF_PID=""
cleanup() {
  status=$?
  trap - EXIT
  for pid in "$CAND_PID" "$REF_PID"; do
    if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi
  done
  for pid in "$CAND_PID" "$REF_PID"; do
    if [ -n "$pid" ]; then wait "$pid" 2>/dev/null || true; fi
  done
  if [ -d "$REF_WORKTREE" ]; then git worktree remove "$REF_WORKTREE" || status=1; fi
  if [ "$status" -eq 0 ]; then
    rm -rf "$WORK"
  else
    echo "Regression artifacts retained at $WORK" >&2
    for log in "$WORK"/*.log; do [ ! -f "$log" ] || tail -n 30 "$log" >&2; done
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

LATEST_TAG=${NATIVE_REF_TAG:-$(gh release view --json tagName --jq .tagName)}
echo "Reference release: $LATEST_TAG"
go build -ldflags="-s -w" -o "$WORK/candidate.bin" ./cmd/server
git fetch --no-tags origin "refs/tags/$LATEST_TAG:refs/tags/$LATEST_TAG"
git worktree add --detach "$REF_WORKTREE" "$LATEST_TAG"
(cd "$REF_WORKTREE" && go build -ldflags="-s -w" -o "$WORK/released.bin" ./cmd/server)
git worktree remove "$REF_WORKTREE"

read -r CAND_PORT REF_PORT < <(python3 - <<'PY'
import socket
sockets = [socket.socket(), socket.socket()]
for sock in sockets:
    sock.bind(("127.0.0.1", 0))
print(*(sock.getsockname()[1] for sock in sockets))
for sock in sockets:
    sock.close()
PY
)
YEAR=$(date +%Y)
TOKEN=native-regression
start_server() {
  name=$1 port=$2 binary=$3
  mkdir "$WORK/$name"
  PORT="$port" CACHE_DB_PATH="$WORK/$name/cache.db" \
    MAPPING_PATH="$WORK/$name/mappings.json.zst" PREWARM_YEARS="$YEAR" \
    INCLUDE_TYPES=TV,ONA EXCLUDE_TAGS='' FILTER_FUTURE_ENABLED=true \
    DEBUG_ENDPOINTS_ENABLED=true ADMIN_TOKEN="$TOKEN" LOG_LEVEL=info \
    "$binary" >"$WORK/$name.log" 2>&1 &
  STARTED_PID=$!
}
start_server candidate "$CAND_PORT" "$WORK/candidate.bin"
CAND_PID=$STARTED_PID
start_server released "$REF_PORT" "$WORK/released.bin"
REF_PID=$STARTED_PID

request() {
  port=$1 method=$2 path=$3 expected=$4 output=$5
  code=$(curl -sS --max-time 100 -X "$method" -H "Authorization: Bearer $TOKEN" \
    -D "$output.headers" -o "$output" -w '%{http_code}' "http://127.0.0.1:$port$path")
  if [ "$code" != "$expected" ]; then
    echo "$method $path returned $code, expected $expected" >&2
    cat "$output" >&2
    return 1
  fi
}
wait_for_entries() {
  port=$1 entries=$2 pid=$3
  for _ in $(seq 1 90); do
    kill -0 "$pid" || { echo "Server exited before cache readiness" >&2; return 1; }
    if curl -fsS --max-time 2 -H "Authorization: Bearer $TOKEN" \
      "http://127.0.0.1:$port/cache/stats" 2>/dev/null | jq -e ".entries >= $entries" >/dev/null; then return; fi
    sleep 1
  done
  echo "Cache did not reach $entries entries within the readiness window" >&2
  return 1
}
for port in "$CAND_PORT" "$REF_PORT"; do
  pid=$CAND_PID
  [ "$port" != "$REF_PORT" ] || pid=$REF_PID
  wait_for_entries "$port" 1 "$pid"
  request "$port" GET "/list?season=WINTER&year=$YEAR" 200 "$WORK/warm-$port.json"
  wait_for_entries "$port" 2 "$pid"
  request "$port" GET /health 200 "$WORK/health-$port.json"
  jq -e '.status == "ok" and .checks.cache.status == "ok" and .checks.resolver.status == "ok"' "$WORK/health-$port.json" >/dev/null
  request "$port" GET '/list?season=INVALID' 400 "$WORK/error-$port"
  request "$port" GET '/list?year=abc' 400 "$WORK/error-$port"
  request "$port" GET '/list?category=invalid' 400 "$WORK/error-$port"
  request "$port" POST /list 405 "$WORK/error-$port"
  grep -qi '^Allow: GET, HEAD' "$WORK/error-$port.headers"
  request "$port" GET /cache/clear 405 "$WORK/error-$port"
  grep -qi '^Allow: POST' "$WORK/error-$port.headers"
  code=$(curl -sS --max-time 5 -o "$WORK/unauthorized-$port" -w '%{http_code}' "http://127.0.0.1:$port/cache/stats")
  [ "$code" = 404 ] || { echo "Missing admin token returned $code" >&2; exit 1; }
done

result=0
for season in ALL WINTER SPRING SUMMER FALL; do
  for category in series series-new; do
    for name in candidate released; do
      port=$CAND_PORT
      [ "$name" != released ] || port=$REF_PORT
      output="$WORK/$name-$season-$category.json"
      request "$port" GET "/list?season=$season&year=$YEAR&category=$category" 200 "$output"
      jq -e 'type == "array" and all(.[]; (.tvdbId | type == "number") and .tvdbId > 0 and (.title | type == "string"))' "$output" >/dev/null
      if [ "$season/$category" = ALL/series ]; then jq -e 'length > 0' "$output" >/dev/null; fi
      jq -S . "$output" > "$output.normalized"
    done
    echo "Comparing $season/$category (IDs, titles, and ordering)"
    if ! diff -u "$WORK/released-$season-$category.json.normalized" "$WORK/candidate-$season-$category.json.normalized"; then result=1; fi
  done
done

for port in "$CAND_PORT" "$REF_PORT"; do
  code=$(curl -sS --max-time 5 -I -o "$WORK/head-$port" -w '%{http_code}' "http://127.0.0.1:$port/health")
  [ "$code" = 200 ]
  request "$port" POST /cache/clear 200 "$WORK/clear-$port.json"
  jq -e '.status == "ok"' "$WORK/clear-$port.json" >/dev/null
  request "$port" GET /cache/stats 200 "$WORK/stats-$port.json"
  jq -e '.entries == 0' "$WORK/stats-$port.json" >/dev/null
done

kill "$CAND_PID" "$REF_PID"
for _ in $(seq 1 20); do
  if ! kill -0 "$CAND_PID" 2>/dev/null && ! kill -0 "$REF_PID" 2>/dev/null; then break; fi
  sleep 1
done
for pid in "$CAND_PID" "$REF_PID"; do
  if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid"; echo "Shutdown exceeded 20 seconds" >&2; exit 1; fi
  wait "$pid" || { echo "Server shutdown failed" >&2; exit 1; }
done
CAND_PID=""
REF_PID=""
if [ "$result" -ne 0 ]; then
  echo "Response differences require review; upstream drift is not automatically accepted." >&2
  exit 1
fi
echo "PASS: all seasons/categories and endpoint/shutdown assertions match $LATEST_TAG"
