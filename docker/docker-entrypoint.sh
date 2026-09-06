#!/bin/sh
set -eu

CONFIG_PATH="${MKV_PROXY_CONFIG_PATH:-/config.json}"
ROOT_PATH="${TIRAMISU_ROOT_PATH:-${GOSTREAM_ROOT_PATH:-/usr/local}}"
SOURCE_PATH="${TIRAMISU_SOURCE_PATH:-${GOSTREAM_SOURCE_PATH:-/mnt/tiramisu-mkv-real}}"
MOUNT_PATH="${TIRAMISU_MOUNT_PATH:-${GOSTREAM_MOUNT_PATH:-/mnt/tiramisu-mkv-virtual}}"
STATE_DIR="${TIRAMISU_STATE_DIR:-${GOSTREAM_STATE_DIR:-$ROOT_PATH/STATE}}"
LOG_DIR="${TIRAMISU_LOG_DIR:-${GOSTREAM_LOG_DIR:-$ROOT_PATH/logs}}"
HOST_MOUNT_HINT="${TIRAMISU_HOST_MOUNT_HINT:-${GOSTREAM_HOST_MOUNT_HINT:-}}"

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

tiramisu_pid=""
stopping=""

# Exit code tiramisu uses to ask for a restart (Control Panel button). Any other
# code ends the container, so a real crash still hands control to Docker's
# restart policy. Keep in sync with restartExitCode in main.go.
RESTART_EXIT_CODE=75

shutdown() {
  trap - INT TERM EXIT
  stopping=1

  if [ -n "$tiramisu_pid" ] && kill -0 "$tiramisu_pid" 2>/dev/null; then
    kill -TERM "$tiramisu_pid" 2>/dev/null || true
  fi

  wait ${tiramisu_pid:+"$tiramisu_pid"} 2>/dev/null || true
  fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true
}

trap shutdown INT TERM EXIT

# Supervision loop: without it a Control Panel restart would stop the container,
# since the entrypoint is PID 1's only child and nothing restarts it.
while :; do
  echo "Starting tiramisu" >&2
  /usr/local/bin/tiramisu --path "$ROOT_PATH" "$SOURCE_PATH" "$MOUNT_PATH" &
  tiramisu_pid="$!"

  exit_code=0
  wait "$tiramisu_pid" || exit_code=$?
  tiramisu_pid=""

  # Unmount between runs, or the next process finds the previous FUSE layer.
  fusermount3 -uz "$MOUNT_PATH" 2>/dev/null || true

  if [ -n "$stopping" ] || [ "$exit_code" -ne "$RESTART_EXIT_CODE" ]; then
    break
  fi
  echo "Restart requested from the Control Panel" >&2
done

exit "$exit_code"
