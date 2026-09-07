#!/bin/sh
set -eu

CONFIG_PATH="${MKV_PROXY_CONFIG_PATH:-/config.json}"
ROOT_PATH="${TIRAMISU_ROOT_PATH:-${GOSTREAM_ROOT_PATH:-/state}}"
SOURCE_PATH="${TIRAMISU_SOURCE_PATH:-${GOSTREAM_SOURCE_PATH:-/mnt/tiramisu-mkv-real}}"
MOUNT_PATH="${TIRAMISU_MOUNT_PATH:-${GOSTREAM_MOUNT_PATH:-/mnt/tiramisu-mkv-virtual}}"
STATE_DIR="${TIRAMISU_STATE_DIR:-${GOSTREAM_STATE_DIR:-$ROOT_PATH/STATE}}"
LOG_DIR="${TIRAMISU_LOG_DIR:-${GOSTREAM_LOG_DIR:-$ROOT_PATH/logs}}"
HOST_MOUNT_HINT="${TIRAMISU_HOST_MOUNT_HINT:-${GOSTREAM_HOST_MOUNT_HINT:-}}"

mkdir -p "$SOURCE_PATH" "$MOUNT_PATH" "$ROOT_PATH"

# True when the path sits under a volume or bind mount rather than the container's
# own writable layer. "/" is the overlay itself and does not count.
path_is_persistent() {
  p=$(cd "$1" 2>/dev/null && pwd -P) || return 1
  while [ "$p" != "/" ] && [ -n "$p" ]; do
    if awk -v d="$p" '$2 == d { found = 1 } END { exit !found }' /proc/mounts 2>/dev/null; then
      return 0
    fi
    p=$(dirname "$p")
  done
  return 1
}

# Images before v1.9.61 defaulted ROOT_PATH to /usr/local, inside the container's
# writable layer. Carry that state over so an in-place image update keeps the peer
# port, the torrent list and the settings instead of silently starting from defaults.
LEGACY_ROOT="/usr/local"
dir_is_empty() {
  [ -z "$(ls -A "$1" 2>/dev/null)" ]
}
if [ "$ROOT_PATH" != "$LEGACY_ROOT" ] && path_is_persistent "$ROOT_PATH"; then
  for entry in config.db settings.json accs.db trackers.txt blocklist; do
    if [ -f "$LEGACY_ROOT/$entry" ] && [ ! -e "$ROOT_PATH/$entry" ]; then
      echo "Migrating $LEGACY_ROOT/$entry to $ROOT_PATH/$entry" >&2
      mv "$LEGACY_ROOT/$entry" "$ROOT_PATH/$entry" 2>/dev/null \
        || cp -a "$LEGACY_ROOT/$entry" "$ROOT_PATH/$entry" 2>/dev/null \
        || echo "WARNING: could not migrate $entry" >&2
    fi
  done
  # STATE/ and logs/ may already exist as empty directories; move their contents.
  for entry in STATE logs; do
    src="$LEGACY_ROOT/$entry"
    dst="$ROOT_PATH/$entry"
    if [ -d "$src" ] && ! dir_is_empty "$src" && { [ ! -d "$dst" ] || dir_is_empty "$dst"; }; then
      echo "Migrating $src/ to $dst/" >&2
      mkdir -p "$dst"
      cp -a "$src/." "$dst/" 2>/dev/null && rm -rf "$src" \
        || echo "WARNING: could not migrate $entry/" >&2
    fi
  done
fi

mkdir -p "$STATE_DIR" "$LOG_DIR"

# GoStorm keeps its settings in $ROOT_PATH/config.db and Tiramisu its state in
# $STATE_DIR. Unmounted, both vanish on "docker rm" and every setting silently
# returns to its default, which is hard to attribute after the fact.
for dir in "$ROOT_PATH" "$STATE_DIR"; do
  if ! path_is_persistent "$dir"; then
    echo "===============================================================" >&2
    echo "WARNING: $dir is NOT on a mounted volume." >&2
    echo "" >&2
    echo "  Torrents, the peer port and every other setting stored there" >&2
    echo "  are DISCARDED when this container is removed. A later" >&2
    echo "  'docker rm' + 'docker run' comes back with the defaults." >&2
    echo "" >&2
    echo "  Fix it by mounting a host directory on $ROOT_PATH:" >&2
    echo "    -v /opt/tiramisu/state:$ROOT_PATH" >&2
    echo "===============================================================" >&2
  fi
done

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

# Tiramisu logs to stdout only; under systemd the unit appends that to a file, but a
# container has nothing doing so, which left the Control Panel's log viewer empty while
# "docker logs" worked. A FIFO feeds tee, which writes both destinations - and keeps $!
# pointing at tiramisu, which a plain pipeline would not.
TIRAMISU_LOG_FILE="$LOG_DIR/tiramisu.log"
LOG_FIFO="$(mktemp -u /tmp/tiramisu-log.XXXXXX)"
if mkfifo "$LOG_FIFO" 2>/dev/null; then
  tee -a "$TIRAMISU_LOG_FILE" < "$LOG_FIFO" &
  tee_pid="$!"
  # Hold a writer open for the life of the script. Without it tee sees EOF when
  # tiramisu exits, and the next run blocks forever opening a FIFO nobody reads.
  exec 3> "$LOG_FIFO"
else
  echo "WARNING: could not create the log FIFO; logs go to stdout only." >&2
  LOG_FIFO=""
fi

# Supervision loop: without it a Control Panel restart would stop the container,
# since the entrypoint is PID 1's only child and nothing restarts it.
while :; do
  echo "Starting tiramisu" >&2
  if [ -n "$LOG_FIFO" ]; then
    /usr/local/bin/tiramisu --path "$ROOT_PATH" "$SOURCE_PATH" "$MOUNT_PATH" > "$LOG_FIFO" 2>&1 &
  else
    /usr/local/bin/tiramisu --path "$ROOT_PATH" "$SOURCE_PATH" "$MOUNT_PATH" &
  fi
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

if [ -n "$LOG_FIFO" ]; then
  exec 3>&-
  wait "$tee_pid" 2>/dev/null || true
  rm -f "$LOG_FIFO"
fi

exit "$exit_code"
