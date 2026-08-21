#!/bin/sh
set -eu

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
	echo "usage: $0 IMAGE_TAG" >&2
	exit 2
fi

IMAGE_TAG=$1
SMOKE_DIR=$(mktemp -d "${TMPDIR:-/tmp}/sonarr-anime-bridge-runtime.XXXXXX")
MAPPING_PID=
WRITABLE_CONTAINER="runtime-smoke-writable-$$"
UNWRITABLE_CONTAINER="runtime-smoke-unwritable-$$"

print_logs() {
	name=$1
	if [ -n "$name" ]; then
		echo "--- docker logs: $name ---" >&2
		docker logs "$name" >&2 2>&1 || :
	fi
}

cleanup() {
	status=$?
	trap - EXIT
	if [ "$status" -ne 0 ]; then
		print_logs "$WRITABLE_CONTAINER"
		print_logs "$UNWRITABLE_CONTAINER"
	fi
	if [ -n "$MAPPING_PID" ]; then
		kill "$MAPPING_PID" 2>/dev/null || :
		wait "$MAPPING_PID" 2>/dev/null || :
	fi
	docker rm -f "$WRITABLE_CONTAINER" >/dev/null 2>&1 || :
	docker rm -f "$UNWRITABLE_CONTAINER" >/dev/null 2>&1 || :
	rm -rf "$SMOKE_DIR"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 2' HUP INT TERM

mapping_fixture=$SMOKE_DIR/mappings.json.zst
mapping_port_file=$SMOKE_DIR/mapping-port
mapping_log=$SMOKE_DIR/mapping-server.log
writable_data=$SMOKE_DIR/writable-data
unwritable_data=$SMOKE_DIR/unwritable-data
mkdir -p "$writable_data" "$unwritable_data"

# This is the smallest valid compressed mapping accepted by the resolver. The
# local server below makes the smoke independent of release data and DNS.
python3 - "$mapping_fixture" <<'PY'
import base64
import sys

fixture = (
    "KLUv/QRYXQIAYoQPFpA7B4ifmM2xejG7W06r2mQd6tZ/LAGZWfdFRESU7tpii1EFMl/"
    "kwH2v24ckIP9plHYgieP63sJIhM/U0hcDAIDQrpJqwkoZlUsh8w=="
)
with open(sys.argv[1], "wb") as output:
    output.write(base64.b64decode(fixture))
PY

python3 - "$mapping_fixture" "$mapping_port_file" >"$mapping_log" 2>&1 <<'PY' &
import http.server
import pathlib
import sys

fixture = pathlib.Path(sys.argv[1]).read_bytes()
port_file = pathlib.Path(sys.argv[2])

class Handler(http.server.BaseHTTPRequestHandler):
    def do_HEAD(self):
        self.send_file(False)

    def do_GET(self):
        self.send_file(True)

    def send_file(self, body):
        if self.path != "/mappings.json.zst":
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(len(fixture)))
        self.send_header("ETag", '"runtime-smoke-v1"')
        self.end_headers()
        if body:
            self.wfile.write(fixture)

    def log_message(self, *_):
        pass

server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
port_file.write_text(str(server.server_port))
server.serve_forever()
PY
MAPPING_PID=$!

attempt=0
while [ ! -s "$mapping_port_file" ] && [ "$attempt" -lt 30 ]; do
	sleep 1
	attempt=$((attempt + 1))
done
if [ ! -s "$mapping_port_file" ]; then
	echo "controlled mapping server did not start" >&2
	exit 1
fi
MAPPING_PORT=$(cat "$mapping_port_file")

APP_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
UNWRITABLE_APP_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
if [ "$UNWRITABLE_APP_PORT" = "$APP_PORT" ]; then
	echo "failed to allocate distinct scenario ports" >&2
	exit 1
fi
MAPPING_URL="http://127.0.0.1:$MAPPING_PORT/mappings.json.zst"

# The image's nonroot user is 65532:65532. Keep the host fixture owned by
# that identity so this tests the same bind-mount contract as production.
if ! chown 65532:65532 "$writable_data" 2>/dev/null; then
	sudo -n chown 65532:65532 "$writable_data"
fi
chmod 0755 "$writable_data"
chmod 0555 "$unwritable_data"

docker run -d --name "$WRITABLE_CONTAINER" --network host --user 65532:65532 \
	-e "PORT=$APP_PORT" \
	-e "PREWARM_YEARS=2026" \
	-e "MAPPING_URL=$MAPPING_URL" \
	-e ALLOW_INSECURE_MAPPING_URL=1 \
	-v "$writable_data:/data" \
	"$IMAGE_TAG" >/dev/null

wait_for_healthy() {
	attempt=0
	while [ "$attempt" -lt 60 ]; do
		writable_state=$(docker inspect --format '{{.State.Status}}' "$WRITABLE_CONTAINER" 2>/dev/null || printf 'missing')
		if [ "$writable_state" = exited ] || [ "$writable_state" = dead ] || [ "$writable_state" = missing ]; then
			echo "writable container stopped before becoming healthy" >&2
		exit 1
		fi
	health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$WRITABLE_CONTAINER" 2>/dev/null || printf 'missing')
		if [ "$health" = healthy ]; then
			return 0
		fi
		sleep 1
		attempt=$((attempt + 1))
	done
	echo "writable container did not become healthy within 60 seconds" >&2
	exit 1
}
wait_for_healthy

health_body=$SMOKE_DIR/health.json
http_code=$(curl -sS --max-time 5 -o "$health_body" -w '%{http_code}' "http://127.0.0.1:$APP_PORT/health" || printf '000')
if [ "$http_code" != 200 ]; then
	echo "health endpoint returned HTTP $http_code" >&2
	exit 1
fi
python3 - "$health_body" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    payload = json.load(source)
if payload.get("status") != "ok":
    raise SystemExit("health status is not ok")
checks = payload.get("checks")
if not isinstance(checks, dict) or set(checks) != {"cache", "resolver"}:
    raise SystemExit("health checks shape is invalid")
cache = checks["cache"]
if not isinstance(cache, dict) or set(cache) != {"status"} or cache.get("status") not in {"ok", "warming"}:
    raise SystemExit("cache health status is invalid")
if checks["resolver"] != {"status": "ok"}:
    raise SystemExit("resolver health shape is invalid")
allowed = {"status", "checks", "reason"}
if set(payload) - allowed:
    raise SystemExit("health response contains unexpected fields")
PY

if [ ! -s "$writable_data/cache.db" ]; then
	echo "writable scenario did not create cache.db" >&2
	exit 1
fi
if [ ! -s "$writable_data/anibridge_mappings.json.zst" ]; then
	echo "writable scenario did not persist mapping data" >&2
	exit 1
fi
if [ ! -s "$writable_data/anibridge_mappings.json.zst.meta.json" ]; then
	echo "writable scenario did not persist mapping metadata" >&2
	exit 1
fi

# A rejected mount must fail before SQLite, mapping, or the write probe can
# create anything in the directory.
docker run -d --name "$UNWRITABLE_CONTAINER" --network host --user 65532:65532 \
	-e "PORT=$UNWRITABLE_APP_PORT" \
	-e "MAPPING_URL=$MAPPING_URL" \
	-e ALLOW_INSECURE_MAPPING_URL=1 \
	-v "$unwritable_data:/data" \
	"$IMAGE_TAG" >/dev/null

attempt=0
while [ "$attempt" -lt 30 ]; do
	unwritable_state=$(docker inspect --format '{{.State.Status}}' "$UNWRITABLE_CONTAINER" 2>/dev/null || printf 'missing')
	if [ "$unwritable_state" = exited ] || [ "$unwritable_state" = dead ]; then
		break
	fi
	if curl -sS --max-time 1 "http://127.0.0.1:$UNWRITABLE_APP_PORT/health" >/dev/null 2>&1; then
		echo "unwritable container served requests before rejecting /data" >&2
		exit 1
	fi
	sleep 1
	attempt=$((attempt + 1))
done
if [ "$unwritable_state" != exited ] && [ "$unwritable_state" != dead ]; then
	echo "unwritable container did not exit within 30 seconds" >&2
	exit 1
fi
exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$UNWRITABLE_CONTAINER")
if [ "$exit_code" -eq 0 ]; then
	echo "unwritable container exited successfully" >&2
	exit 1
fi
unwritable_logs=$SMOKE_DIR/unwritable.log
docker logs "$UNWRITABLE_CONTAINER" >"$unwritable_logs" 2>&1 || :
if ! python3 - "$unwritable_logs" <<'PY'
import sys

with open(sys.argv[1], encoding="utf-8", errors="replace") as source:
    logs = source.read()
if "CACHE_DB_PATH/MAPPING_PATH directory" not in logs:
    raise SystemExit(1)
PY
then
	echo "unwritable container did not report runtime-directory validation failure" >&2
	exit 1
fi

leftover=$(find "$unwritable_data" -type f -print)
if [ -n "$leftover" ]; then
	echo "unwritable scenario created unexpected file: $leftover" >&2
	exit 1
fi

echo "runtime smoke passed for $IMAGE_TAG"
