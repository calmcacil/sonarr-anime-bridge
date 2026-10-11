#!/bin/sh
# Package/kernel provisioning only; tested engines run as the runner account.
set -eu
test "${GITHUB_ACTIONS:-}" = true || { echo 'This provisioning script is for disposable GitHub runners' >&2; exit 1; }
test "$(id -u)" -ne 0
sudo apt-get update -qq
sudo apt-get install -y podman uidmap slirp4netns fuse-overlayfs dbus-user-session
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tools=$(mktemp -d "${TMPDIR:-/tmp}/sab-rootless-tools.XXXXXX")
sh "$root/scripts/install-rootless-docker.sh" "$tools"
printf 'SAB_ROOTLESS_DOCKER_BIN=%s/bin\n' "$tools" >> "$GITHUB_ENV"
# Ubuntu 24.04 restricts unprivileged namespace creation through AppArmor.
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
user=$(id -un)
if ! grep -q "^$user:" /etc/subuid; then sudo usermod --add-subuids 100000-165535 "$user"; fi
if ! grep -q "^$user:" /etc/subgid; then sudo usermod --add-subgids 100000-165535 "$user"; fi
dpkg-query -W podman uidmap slirp4netns fuse-overlayfs
docker --version
podman --version
