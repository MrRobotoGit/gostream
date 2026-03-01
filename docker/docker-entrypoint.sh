#!/bin/sh
set -eu

CONFIG_PATH="${MKV_PROXY_CONFIG_PATH:-/config.json}"
SOURCE_PATH="${GOSTREAM_SOURCE_PATH:-/mnt/gostream-mkv-real}"
MOUNT_PATH="${GOSTREAM_MOUNT_PATH:-/mnt/gostream-mkv-virtual}"

mkdir -p "$SOURCE_PATH" "$MOUNT_PATH"

# Clean up any stale FUSE mount left by a previous crash. Without this,
# fs.Mount() will fail with "transport endpoint is not connected".
# -u unmounts, -z defers cleanup until the mountpoint is no longer busy.
if mountpoint -q "$MOUNT_PATH" 2>/dev/null; then
  echo "Stale FUSE mount detected at $MOUNT_PATH, cleaning up..." >&2
  fusermount3 -uz "$MOUNT_PATH" || true
fi

if [ ! -f "$CONFIG_PATH" ]; then
  echo "Missing required config file at $CONFIG_PATH" >&2
  exit 1
fi

exec /usr/local/bin/gostream "$SOURCE_PATH" "$MOUNT_PATH"
