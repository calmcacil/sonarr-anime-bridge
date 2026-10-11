#!/bin/sh
# Start an isolated unprivileged engine; provision packages separately in CI.
set -eu
if [ "$#" -lt 2 ]; then
    echo "usage: $0 docker|podman IMAGE [ARCHIVE EXPECTED_INSPECTION]" >&2
    exit 2
fi
engine=$1
image=$2
archive=${3:-}
expected=${4:-}
case "$engine" in docker|podman) ;; *) echo "unsupported engine: $engine" >&2; exit 2 ;; esac
test "$(id -u)" -ne 0 || { echo 'rootless checks must run as an ordinary user' >&2; exit 1; }
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/sab-rootless-$engine.XXXXXX")
daemon_pid=
cleanup() {
    status=$?
    trap - EXIT
    if [ "$status" -ne 0 ] && [ -f "$work/daemon.log" ]; then tail -80 "$work/daemon.log" >&2; fi
    if [ -n "$daemon_pid" ]; then
        kill "$daemon_pid" 2>/dev/null || :
        wait "$daemon_pid" 2>/dev/null || :
    fi
    # Namespace-owned engine files are disposable. Never repair host ownership.
    if [ "$engine" = podman ]; then
        podman --root "$work/storage" --runroot "$work/runroot" --storage-driver vfs \
            unshare rm -rf "$work/storage" >/dev/null 2>&1 || :
    fi
    echo "Isolated engine evidence/state: $work"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -m 700 "$work/runtime"
export XDG_RUNTIME_DIR="$work/runtime"
if [ "$engine" = docker ]; then
    if [ -n "${SAB_ROOTLESS_DOCKER_BIN:-}" ]; then export PATH="$SAB_ROOTLESS_DOCKER_BIN:$PATH"; fi
    command -v dockerd-rootless.sh >/dev/null || { echo 'dockerd-rootless.sh is required' >&2; exit 1; }
    export DOCKER_HOST="unix://$work/runtime/docker.sock"
    export DOCKERD_ROOTLESS_ROOTLESSKIT_STATE_DIR="$work/rootlesskit"
    dockerd-rootless.sh --host "$DOCKER_HOST" --data-root "$work/storage" \
        --exec-root "$work/exec" --pidfile "$work/docker.pid" >"$work/daemon.log" 2>&1 &
    daemon_pid=$!
    ready=false
    for _ in $(seq 1 60); do
        if docker info >/dev/null 2>&1; then ready=true; break; fi
        kill -0 "$daemon_pid" 2>/dev/null || break
        sleep 1
    done
    test "$ready" = true || { echo 'isolated rootless Docker daemon failed' >&2; exit 1; }
    if [ -n "$archive" ]; then docker load --input "$archive"; else docker pull "$image"; fi
    docker image inspect "$image" >"$work/candidate.json"
else
    export SAB_PODMAN_ROOT="$work/storage" SAB_PODMAN_RUNROOT="$work/runroot"
    if [ -n "$archive" ]; then
        podman --root "$work/storage" --runroot "$work/runroot" --storage-driver vfs load --input "$archive"
    elif [ -n "${DOCKER_CONFIG:-}" ] && [ -f "$DOCKER_CONFIG/config.json" ]; then
        podman --root "$work/storage" --runroot "$work/runroot" --storage-driver vfs \
            pull --authfile "$DOCKER_CONFIG/config.json" "$image"
    else
        podman --root "$work/storage" --runroot "$work/runroot" --storage-driver vfs pull "$image"
    fi
    podman --root "$work/storage" --runroot "$work/runroot" --storage-driver vfs image inspect "$image" >"$work/candidate.json"
fi
if [ -n "$archive" ]; then
    test -n "$expected" || { echo 'archive tests require original candidate inspection' >&2; exit 1; }
    python3 - "$expected" "$work/candidate.json" <<'PY'
import json
from pathlib import Path
import sys
before, after = [json.loads(Path(name).read_text())[0] for name in sys.argv[1:]]
def identity(image):
    return (image.get("Id", image.get("ID", "")).removeprefix("sha256:"), image["RootFS"]["Layers"])
if identity(before) != identity(after):
    raise SystemExit("rootless transport changed candidate config or layers")
print("Exact candidate config and layers preserved across archive transport")
PY
fi
python3 "$root/scripts/runtime_smoke.py" --engine "$engine" --rootless "$image"
