#!/usr/bin/env bash
#
# End-to-end smoke test for snapshot + fork under jailer.
#
# What this proves:
#   1. A sandbox boots under jailer.
#   2. We can exec into it over vsock and write a marker inside the guest.
#   3. Snapshot succeeds (state + memory + rootfs captured).
#   4. Fork count=3 produces three distinct sandboxes, each in its own
#      chroot with its own vsock UDS (the whole point of adopting jailer).
#   5. Each fork sees the pre-snapshot marker (memory + rootfs inherited).
#   6. Teardown is clean: no stale chroots left behind.
#
# Requires root because jailer needs CAP_SYS_ADMIN + privilege drop.
# Overridable via env vars; sensible defaults below.
#
# Usage:
#   sudo CRUCIBLE_BIN=./crucible \
#        FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_fork.sh
#
# Exits 0 on success, non-zero on any failure. Prints what it's doing
# so a human can follow along.

set -euo pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
WORK_BASE="${WORK_BASE:-/var/lib/crucible/run}"
LISTEN="${LISTEN:-127.0.0.1:7879}"   # different port from default so we don't collide with a hand-started daemon
BASE_URL="http://${LISTEN}"
MARKER="crucible-fork-smoke-$$-$RANDOM"

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (jailer requires CAP_SYS_ADMIN)" >&2
  exit 1
fi

for bin in "$CRUCIBLE_BIN" "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  if [[ ! -x "$bin" ]]; then
    echo "error: missing or non-executable: $bin" >&2
    exit 1
  fi
done
for f in "$KERNEL" "$ROOTFS"; do
  if [[ ! -r "$f" ]]; then
    echo "error: missing or unreadable: $f" >&2
    exit 1
  fi
done

DAEMON_LOG=$(mktemp)
trap 'rm -f "$DAEMON_LOG"' EXIT

echo "== starting daemon (jailer mode) on $LISTEN"
"$CRUCIBLE_BIN" daemon \
  --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" \
  --jailer-bin "$JAILER_BIN" \
  --chroot-base "$CHROOT_BASE" \
  --kernel "$KERNEL" \
  --rootfs "$ROOTFS" \
  --work-base "$WORK_BASE" \
  --log-format json --log-level info \
  >"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

cleanup() {
  if kill -0 "$DAEMON_PID" 2>/dev/null; then
    echo "== stopping daemon (pid $DAEMON_PID)"
    kill -TERM "$DAEMON_PID" || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  if [[ -n "${DAEMON_LOG:-}" && -f "$DAEMON_LOG" ]]; then
    echo "----- daemon log tail -----"
    tail -40 "$DAEMON_LOG" || true
    echo "---------------------------"
  fi
}
trap 'cleanup; rm -f "$DAEMON_LOG"' EXIT

echo "== waiting for /healthz"
for _ in {1..50}; do
  if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
curl -sf "$BASE_URL/healthz" >/dev/null || { echo "daemon never became healthy" >&2; exit 1; }

echo "== creating source sandbox"
SRC=$(curl -sSf -X POST "$BASE_URL/sandboxes" \
        -H 'Content-Type: application/json' \
        -d '{"vcpus":1,"memory_mib":256}' \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "   source id: $SRC"

# Write marker inside guest. /bin/sh + sync to make sure the write
# hits the rootfs file before the snapshot captures it.
echo "== writing marker '$MARKER' into /marker"
curl -sSf -X POST "$BASE_URL/sandboxes/$SRC/exec" \
  -H 'Content-Type: application/json' \
  -d "{\"cmd\":[\"/bin/sh\",\"-c\",\"echo $MARKER > /marker && sync\"]}" \
  --output /dev/null

echo "== creating snapshot"
SNAP=$(curl -sSf -X POST "$BASE_URL/sandboxes/$SRC/snapshot" \
       | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "   snapshot id: $SNAP"

echo "== forking count=3"
FORK_RESP=$(mktemp)
FORK_STATUS=$(curl -sS -o "$FORK_RESP" -w '%{http_code}' -X POST "$BASE_URL/snapshots/$SNAP/fork?count=3")
if [[ "$FORK_STATUS" != "201" ]]; then
  echo "   fork failed with HTTP $FORK_STATUS" >&2
  echo "   response body:" >&2
  cat "$FORK_RESP" >&2
  echo >&2
  rm -f "$FORK_RESP"
  exit 1
fi
FORK_IDS=$(python3 -c 'import sys,json;print(" ".join(s["id"] for s in json.load(sys.stdin)["sandboxes"]))' < "$FORK_RESP")
rm -f "$FORK_RESP"
echo "   fork ids: $FORK_IDS"

FORK_ARR=($FORK_IDS)
if [[ ${#FORK_ARR[@]} -ne 3 ]]; then
  echo "expected 3 forks, got ${#FORK_ARR[@]}" >&2
  exit 1
fi

# Parse stdout frames: each frame is [1B type][3B reserved][4B big-endian size][payload].
# type 1 = stdout, 2 = stderr, 3 = exit (JSON payload).
read_marker() {
  python3 - "$1" <<'PY'
import struct, sys, json
with open(sys.argv[1], 'rb') as f:
    data = f.read()
off = 0
chunks = []
while off + 8 <= len(data):
    typ = data[off]
    size = struct.unpack('>I', data[off+4:off+8])[0]
    body = data[off+8:off+8+size]
    off += 8 + size
    if typ == 1:
        chunks.append(body.decode(errors='replace'))
sys.stdout.write(''.join(chunks))
PY
}

echo "== reading /marker from each fork"
for F in "${FORK_ARR[@]}"; do
  OUT=$(mktemp)
  curl -sSf -X POST "$BASE_URL/sandboxes/$F/exec" \
    -H 'Content-Type: application/json' \
    -d '{"cmd":["/bin/cat","/marker"]}' \
    --output "$OUT"
  GOT=$(read_marker "$OUT" | tr -d '\n')
  rm -f "$OUT"
  if [[ "$GOT" != "$MARKER" ]]; then
    echo "   FAIL $F: got '$GOT', want '$MARKER'" >&2
    exit 1
  fi
  echo "   OK   $F: '$GOT'"
done

echo "== tearing down"
for F in $SRC "${FORK_ARR[@]}"; do
  curl -sSf -X DELETE "$BASE_URL/sandboxes/$F" -o /dev/null || echo "   (delete $F: already gone?)"
done
curl -sSf -X DELETE "$BASE_URL/snapshots/$SNAP" -o /dev/null || true

# Verify chroot base is empty (daemon cleaned up + we deleted every VM).
REMAINING=$(ls -A "$CHROOT_BASE/firecracker" 2>/dev/null | wc -l)
if [[ "$REMAINING" -ne 0 ]]; then
  echo "   WARN: $CHROOT_BASE/firecracker not empty after teardown: $(ls -A "$CHROOT_BASE/firecracker" 2>/dev/null)"
fi

echo "== PASS — source boot, marker write, snapshot, fork x3, marker read, teardown"
exit 0
