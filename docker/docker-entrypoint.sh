#!/bin/sh
set -eu

CONFIG_PATH="${MKV_PROXY_CONFIG_PATH:-/config.json}"
ROOT_PATH="${GOSTREAM_ROOT_PATH:-/usr/local}"
SOURCE_PATH="${GOSTREAM_SOURCE_PATH:-/mnt/gostream-mkv-real}"
MOUNT_PATH="${GOSTREAM_MOUNT_PATH:-/mnt/gostream-mkv-virtual}"
STATE_DIR="${GOSTREAM_STATE_DIR:-$ROOT_PATH/STATE}"
LOG_DIR="${GOSTREAM_LOG_DIR:-$ROOT_PATH/logs}"
HOST_MOUNT_HINT="${GOSTREAM_HOST_MOUNT_HINT:-}"

mkdir -p "$SOURCE_PATH" "$MOUNT_PATH" "$ROOT_PATH" "$STATE_DIR" "$LOG_DIR"

mount_is_readable() {
  ls -ld "$1" >/dev/null 2>&1
}

emit_lazy_unmount_hint() {
  hint_path="$MOUNT_PATH"
  if [ -n "$HOST_MOUNT_HINT" ]; then
    hint_path="$HOST_MOUNT_HINT"
  fi

  echo "Mount at $MOUNT_PATH is stale or unreadable." >&2
  echo "If Docker restart/start still fails, lazily unmount the host bind source and retry:" >&2
  echo "  sudo umount -l $hint_path" >&2
}

# Clean up only stale FUSE layers at startup.
# Docker's bind mount for $MOUNT_PATH is always a mountpoint, so a bare
# mountpoint check is not enough. We must leave the bind mount intact and only
# remove an inherited fuse.* layer sitting on top of it.
if mountpoint -q "$MOUNT_PATH" 2>/dev/null; then
  if grep -q " $MOUNT_PATH fuse" /proc/mounts 2>/dev/null; then
    if mount_is_readable "$MOUNT_PATH"; then
      echo "Stale FUSE mount detected at $MOUNT_PATH, cleaning up..." >&2
    else
      echo "Unreadable stale FUSE mount detected at $MOUNT_PATH, cleaning up..." >&2
      emit_lazy_unmount_hint
    fi
    fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
    if ! mount_is_readable "$MOUNT_PATH"; then
      emit_lazy_unmount_hint
      exit 1
    fi
  else
    echo "Non-FUSE mountpoint at $MOUNT_PATH (Docker bind), leaving intact." >&2
  fi
fi

if [ ! -f "$CONFIG_PATH" ]; then
  echo "Missing required config file at $CONFIG_PATH" >&2
  exit 1
fi

gostream_pid=""

shutdown() {
  trap - INT TERM EXIT

  if [ -n "$gostream_pid" ] && kill -0 "$gostream_pid" 2>/dev/null; then
    kill -TERM "$gostream_pid" 2>/dev/null || true
  fi

  wait ${gostream_pid:+"$gostream_pid"} 2>/dev/null || true
  fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
}

trap shutdown INT TERM EXIT

echo "Starting gostream" >&2
/usr/local/bin/gostream --path "$ROOT_PATH" "$SOURCE_PATH" "$MOUNT_PATH" &
gostream_pid="$!"

wait "$gostream_pid"
exit_code=$?
fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
exit "$exit_code"
