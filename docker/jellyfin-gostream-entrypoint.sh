#!/bin/sh
set -eu

CONFIG_PATH="${MKV_PROXY_CONFIG_PATH:-/config/config.json}"
ROOT_PATH="${GOSTREAM_ROOT_PATH:-/config/gostream}"
SOURCE_PATH="${GOSTREAM_SOURCE_PATH:-/mnt/gostream-mkv-real}"
MOUNT_PATH="${GOSTREAM_MOUNT_PATH:-/mnt/gostream-mkv-virtual}"
STATE_DIR="${GOSTREAM_STATE_DIR:-$ROOT_PATH/STATE}"
LOG_DIR="${GOSTREAM_LOG_DIR:-$ROOT_PATH/logs}"
HEALTH_MONITOR_ENABLED="${GOSTREAM_HEALTH_MONITOR_ENABLED:-1}"
HEALTH_MONITOR_PORT="${HEALTH_MONITOR_PORT:-8095}"

JELLYFIN_DATA_DIR="${JELLYFIN_DATA_DIR:-/config/data}"
JELLYFIN_CONFIG_DIR="${JELLYFIN_CONFIG_DIR:-/config/config}"
JELLYFIN_CACHE_DIR="${JELLYFIN_CACHE_DIR:-/cache}"
JELLYFIN_LOG_DIR="${JELLYFIN_LOG_DIR:-/config/log}"

mkdir -p "$SOURCE_PATH" "$MOUNT_PATH" "$ROOT_PATH" "$STATE_DIR" "$LOG_DIR" "$JELLYFIN_DATA_DIR" "$JELLYFIN_CONFIG_DIR" "$JELLYFIN_CACHE_DIR" "$JELLYFIN_LOG_DIR"

if [ ! -f "$CONFIG_PATH" ]; then
  if [ -f /app/config.json.example ]; then
    cp /app/config.json.example "$CONFIG_PATH"
    chmod 0644 "$CONFIG_PATH"
  else
    echo "Missing required config file at $CONFIG_PATH" >&2
    exit 1
  fi
fi

if mountpoint -q "$MOUNT_PATH" 2>/dev/null; then
  if grep -q " $MOUNT_PATH fuse" /proc/mounts 2>/dev/null; then
    fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
  fi
fi

health_pid=""
gostream_pid=""
jellyfin_pid=""

shutdown() {
  trap - INT TERM EXIT

  if [ -n "$health_pid" ] && kill -0 "$health_pid" 2>/dev/null; then
    kill "$health_pid" 2>/dev/null || true
  fi

  if [ -n "$gostream_pid" ] && kill -0 "$gostream_pid" 2>/dev/null; then
    kill -TERM "$gostream_pid" 2>/dev/null || true
  fi

  if [ -n "$jellyfin_pid" ] && kill -0 "$jellyfin_pid" 2>/dev/null; then
    kill -TERM "$jellyfin_pid" 2>/dev/null || true
  fi

  wait ${health_pid:+"$health_pid"} ${gostream_pid:+"$gostream_pid"} ${jellyfin_pid:+"$jellyfin_pid"} 2>/dev/null || true
  fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
}

trap shutdown INT TERM EXIT

if [ "$HEALTH_MONITOR_ENABLED" = "1" ]; then
  python3 /app/scripts/health-monitor.py &
  health_pid="$!"
fi

/usr/local/bin/gostream --path "$ROOT_PATH" "$SOURCE_PATH" "$MOUNT_PATH" &
gostream_pid="$!"

/jellyfin/jellyfin --datadir "$JELLYFIN_DATA_DIR" --configdir "$JELLYFIN_CONFIG_DIR" --cachedir "$JELLYFIN_CACHE_DIR" --logdir "$JELLYFIN_LOG_DIR" --service &
jellyfin_pid="$!"

while :; do
  if [ -n "$health_pid" ] && ! kill -0 "$health_pid" 2>/dev/null; then
    wait "$health_pid" || true
    exit 1
  fi

  if ! kill -0 "$gostream_pid" 2>/dev/null; then
    wait "$gostream_pid" || true
    exit 1
  fi

  if ! kill -0 "$jellyfin_pid" 2>/dev/null; then
    wait "$jellyfin_pid" || true
    exit 1
  fi

  sleep 1
done
