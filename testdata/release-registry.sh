#!/bin/sh
# Exercise real OCI index copies without publishing anything to GHCR.
set -eu

root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
workdir=$(mktemp -d "${TMPDIR:-/tmp}/sonarr-release-registry.XXXXXX")
name="sonarr-release-registry-$$"
builder="sonarr-release-test-$$"
cleanup() {
    status=$?
    trap - EXIT INT TERM
    if [ "$status" -ne 0 ]; then docker logs "$name" >&2 || true; fi
    docker buildx rm "$builder" >/dev/null 2>&1 || true
    docker rm -fv "$name" >/dev/null 2>&1 || true
    rm -rf "$workdir"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker run -d --name "$name" -p 127.0.0.1::5000 \
    registry:3.1.2@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8 >/dev/null
port=$(docker port "$name" 5000/tcp | sed 's/.*://')
image="localhost:$port/sonarr-release-test"
ready=false
for _ in $(seq 1 30); do
    if curl --fail --silent "http://localhost:$port/v2/" >/dev/null; then ready=true; break; fi
    sleep 1
done
test "$ready" = true
docker buildx create --name "$builder" --driver docker-container --driver-opt network=host >/dev/null

# This fixture tests registry/config/attestation handling, not application runtime.
# Native runtime tests use the production image in the CI container matrix.
printf 'registry promotion fixture\n' > "$workdir/server"
mkdir "$workdir/data"
printf 'data fixture\n' > "$workdir/data/fixture"
cat > "$workdir/Dockerfile" <<'EOF'
FROM scratch
COPY server /server
COPY --chown=65532:65532 data /data
ARG VERSION
LABEL org.opencontainers.image.revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
LABEL org.opencontainers.image.version=$VERSION
USER 65532
VOLUME ["/data"]
ENTRYPOINT ["/server"]
EOF

for candidate in v2.14.5 v2.14.6 collision; do
    version=$candidate
    if [ "$candidate" = collision ]; then version=v2.14.5; fi
    docker buildx build --builder "$builder" --platform linux/amd64,linux/arm64 \
        --build-arg "VERSION=$version" --provenance=mode=max --sbom=true \
        --label "test.candidate=$candidate" --tag "$image:candidate-$candidate" \
        --push --metadata-file "$workdir/$candidate.json" "$workdir"
done

PYTHONDONTWRITEBYTECODE=1 python3 - "$root" "$workdir" "$image" <<'PY'
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(sys.argv[1]) / "scripts"))
import release_image as release

workdir, image = Path(sys.argv[2]), sys.argv[3]
old, new, collision = [json.loads((workdir / f"{tag}.json").read_text())["containerimage.digest"]
                      for tag in ("v2.14.5", "v2.14.6", "collision")]
sha = "a" * 40
old_index = release.manifest(f"{image}@{old}")
assert release.manifest(f"{image}:absent", absent_ok=True) is None
assert release.manifest(f"{image}:v2.14.5", absent_ok=True) is None
assert release.promote(image, old, sha, "v2.14.5", ["v2.14.5"]) == ["v2.14", "v2", "latest"]
assert release.manifest(f"{image}:v2.14.5") == old_index
assert release.promote(image, new, sha, "v2.14.6", ["v2.14.5", "v2.14.6"]) == ["v2.14", "v2", "latest"]
assert release.promote(image, old, sha, "v2.14.5", ["v2.14.5", "v2.14.6"]) == []
for alias in ("latest", "v2", "v2.14", "v2.14.6"):
    assert release.manifest(f"{image}:{alias}")["digest"] == new
assert release.manifest(f"{image}:v2.14.5") == old_index
try:
    release.promote(image, collision, sha, "v2.14.5", ["v2.14.5"])
except ValueError as error:
    assert "immutable" in str(error)
else:
    raise AssertionError("Immutable collision must fail")
assert release.manifest(f"{image}:v2.14.5") == old_index
print("Disposable registry passed: unchanged indexes/attestations, immutable tags, and monotonic recovery.")
PY
