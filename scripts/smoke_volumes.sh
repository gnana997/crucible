#!/usr/bin/env bash
#
# smoke_volumes.sh — end-to-end check for persistent volumes (V-M1).
#
# Boots a real daemon under jailer with a --volume-dir, then verifies:
#   1. A volume attaches, mounts, and data written to it survives destroying
#      and re-creating the sandbox with the same volume name.
#   2. The single-writer guard refuses a second live sandbox on the same volume.
#   3. Data written + synced survives a HARD KILL of the VM (SIGKILL, no clean
#      unmount) — the volume reached the host backing file (cache_type=Writeback).
#
# Requires: root + KVM, firecracker + jailer + vmlinux + a rootfs whose agent
# has /mount (build with the current crucible-agent), mkfs.ext4, curl, jq-free.
# The volume-dir is placed on the SAME filesystem as the chroot base so volumes
# hardlink into the jail (a cross-fs volume-dir is rejected by design).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_volumes.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7910}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-voltest}"
# Boot an OCI image (not the profile rootfs) so the freshly-built agent — which
# has the /mount endpoint — is injected at conversion. If you prefer the profile
# rootfs, rebuild it with the current crucible-agent first (make rootfs). alpine
# must be pullable (or pre-present in the image store).
IMAGE="${IMAGE:-alpine:latest}"

pass=0; fail=0
ok()   { echo "  ✓ $*"; pass=$((pass+1)); }
bad()  { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible volumes smoke (V-M1)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r "$ROOTFS" ]] || { echo "error: rootfs not readable: $ROOTFS" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null || { echo "error: mkfs.ext4 needed (e2fsprogs)" >&2; exit 2; }
if systemctl is-active --quiet crucible 2>/dev/null; then
  echo "error: systemd crucible is active — stop it first (this starts its own daemon)" >&2; exit 2
fi

# ---- work root (all on one fs so volumes hardlink into the jail) ------------
echo "== 01 prepare work root ($MOUNT)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
    --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
    --volume-dir "$MOUNT/volumes" --volume-default-size $((256*1024*1024)) \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never became healthy"; tail -20 "$DAEMON_LOG"; exit 3
}

echo "== 02 start daemon (--volume-dir $MOUNT/volumes)"
start_daemon
echo "   daemon healthy (pid $DAEMON_PID)"

run()  { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }

# ---- 03 persistence across re-create ---------------------------------------
echo "== 03 data survives destroy + re-create with the same volume"
SBX1=$(run sandbox create --image "$IMAGE" --volume data:/vol) || { bad "create #1"; }
if [[ -n "${SBX1:-}" ]]; then
  run sandbox exec "$SBX1" -- sh -c 'echo hello-crucible > /vol/marker && sync' >/dev/null 2>&1 \
    && ok "wrote /vol/marker in sandbox 1" || bad "write to volume"
  run rm "$SBX1" >/dev/null 2>&1 && ok "destroyed sandbox 1 (volume kept)" || bad "rm sandbox 1"

  SBX2=$(run sandbox create --image "$IMAGE" --volume data:/vol)
  GOT=$(run sandbox exec "$SBX2" -- cat /vol/marker 2>/dev/null | tr -d '\r\n')
  [[ "$GOT" == "hello-crucible" ]] && ok "re-created sandbox reads back the data" \
    || bad "expected 'hello-crucible', got '$GOT'"
fi

# ---- 04 single-writer guard -------------------------------------------------
echo "== 04 second live sandbox on the same volume is refused"
if run sandbox create --image "$IMAGE" --volume data:/vol >/dev/null 2>&1; then
  bad "second concurrent attach was NOT refused (single-writer guard broken)"
else
  ok "concurrent attach refused (ext4 single-writer guard)"
fi
run rm "${SBX2:-}" >/dev/null 2>&1 || true

# ---- 05 hard-kill durability (fsync gate) ----------------------------------
echo "== 05 data written + synced survives a HARD KILL of the VM"
SBX3=$(run sandbox create --image "$IMAGE" --volume data:/vol)
if [[ -n "${SBX3:-}" ]]; then
  run sandbox exec "$SBX3" -- sh -c 'echo survive-the-kill > /vol/durable && sync' >/dev/null 2>&1 \
    && ok "wrote + synced /vol/durable" || bad "write+sync"
  # SIGKILL the firecracker/jailer process for this sandbox — no clean unmount.
  pkill -9 -f "firecracker --id" 2>/dev/null || true
  sleep 1
  run rm "$SBX3" >/dev/null 2>&1 || true   # releases the in-mem guard on the dead VM
  SBX4=$(run sandbox create --image "$IMAGE" --volume data:/vol)
  GOT=$(run sandbox exec "$SBX4" -- cat /vol/durable 2>/dev/null | tr -d '\r\n')
  [[ "$GOT" == "survive-the-kill" ]] && ok "synced data survived the hard kill" \
    || bad "expected 'survive-the-kill', got '$GOT'"
  run rm "$SBX4" >/dev/null 2>&1 || true
fi

# ---- 06 explicit lifecycle: create --size, ls, duplicate, rm ---------------
echo "== 06 volume create/ls/rm lifecycle (V-M2)"
run volume create sized --size 128M >/dev/null 2>&1 && ok "volume create --size 128M" || bad "volume create --size"
run volume ls 2>/dev/null | grep -qw sized && ok "volume ls shows 'sized'" || bad "volume ls missing 'sized'"
if run volume create sized --size 128M >/dev/null 2>&1; then bad "duplicate create NOT refused"; else ok "duplicate create refused"; fi
run volume rm sized >/dev/null 2>&1 && ok "volume rm (detached)" || bad "volume rm"

# ---- 07 rm refused while attached, allowed after detach --------------------
echo "== 07 volume rm refused while attached (V-M2)"
SBX5=$(run sandbox create --image "$IMAGE" --volume held:/vol)
if run volume rm held >/dev/null 2>&1; then bad "rm NOT refused while attached"; else ok "rm refused while attached"; fi
run sandbox rm "${SBX5:-}" >/dev/null 2>&1 || true
run volume rm held >/dev/null 2>&1 && ok "rm succeeds after detach" || bad "rm after detach"

# ---- 08 records survive a daemon restart -----------------------------------
echo "== 08 volume records survive a daemon restart (V-M2)"
run volume create persist --size 128M >/dev/null 2>&1 || bad "create persist"
kill -TERM "$DAEMON_PID" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null
start_daemon
run volume ls 2>/dev/null | grep -qw persist && ok "volume survived daemon restart (durable store)" || bad "volume gone after restart"
run volume rm persist >/dev/null 2>&1 || true

echo "==============================================================="
echo " volumes smoke: $pass passed, $fail failed"
echo "==============================================================="
[[ $fail -eq 0 ]]
