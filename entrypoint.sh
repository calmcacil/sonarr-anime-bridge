#!/bin/sh
set -eu

PUID=${PUID:-1000}
PGID=${PGID:-1000}

# Validate that PUID and PGID are numeric before using them in shell commands.
case "$PUID" in
  ''|*[!0-9]*) echo "error: PUID must be numeric, got '$PUID'" >&2; exit 1;;
esac
case "$PGID" in
  ''|*[!0-9]*) echo "error: PGID must be numeric, got '$PGID'" >&2; exit 1;;
esac
if [ "${ALLOW_ROOT:-0}" != "1" ] && { [ "$PUID" = "0" ] || [ "$PGID" = "0" ]; }; then
  echo "error: PUID/PGID 0 requires ALLOW_ROOT=1" >&2
  exit 1
fi

# Resolve the actual group name for PGID, or create "appgroup" if none exists.
GROUP_NAME=$(getent group "$PGID" | cut -d: -f1)
if [ -z "$GROUP_NAME" ]; then
  GROUP_NAME="appgroup"
  addgroup -g "$PGID" "$GROUP_NAME"
fi

# Create the user if it doesn't already exist.
if ! getent passwd "$PUID" > /dev/null 2>&1; then
  adduser -u "$PUID" -G "$GROUP_NAME" -D -h /data appuser
fi

find /data -xdev \( -type f -o -type d \) -exec chown "$PUID:$PGID" {} +

exec su-exec "$PUID:$PGID" /server "$@"
