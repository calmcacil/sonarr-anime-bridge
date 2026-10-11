#!/bin/sh
# Install verified static tools in a private directory, without replacing Docker.
set -eu
test "$#" -eq 1 || { echo 'usage: install-rootless-docker.sh DIRECTORY' >&2; exit 2; }
destination=$1
version=29.9.0
case "$(uname -m)" in
    aarch64|arm64)
        architecture=aarch64
        docker_sha=811f6eee271678fc98dbe5c4d896c00fc5fe2daa4f7a37069c17abc036eab85b
        extras_sha=f1b76a0721101e15c1eabeccf08751c0d42206defe06422de2e4b8897fc0f129
        ;;
    x86_64|amd64)
        architecture=x86_64
        docker_sha=33e1ab8b63d14bca449f7a3d30d7f6aa669daa544e60a86b17fec87f61afe2b4
        extras_sha=6b4a2e085a1e4ade5ba3de99fd770a0a9b53c719e6bf6b1632242bf4b068adb6
        ;;
    *) echo 'Unsupported rootless Docker tool architecture' >&2; exit 1 ;;
esac
mkdir -p "$destination/bin"
base="https://download.docker.com/linux/static/stable/$architecture"
curl --fail --silent --show-error --location "$base/docker-$version.tgz" -o "$destination/docker.tgz"
curl --fail --silent --show-error --location "$base/docker-rootless-extras-$version.tgz" -o "$destination/extras.tgz"
printf '%s  %s\n' "$docker_sha" "$destination/docker.tgz" "$extras_sha" "$destination/extras.tgz" | sha256sum --check --strict
tar -xzf "$destination/docker.tgz" -C "$destination/bin" --strip-components=1
tar -xzf "$destination/extras.tgz" -C "$destination/bin" --strip-components=1
"$destination/bin/docker" --version
"$destination/bin/dockerd" --version
