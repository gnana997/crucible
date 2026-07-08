#!/usr/bin/env bash
#
# Orphan-VMM reaping smoke.
#
# Proves a hard-killed daemon leaves no firecracker running for long: the
# next daemon start reaps the orphan (KillLiveOrphans + ReapOrphans).
#
#   01  daemon up (images); boot a sandbox from an OCI image
#   02  the sandbox's firecracker/jailer is running (by --id token)
#   03  kill -9 the daemon → the VMM is orphaned but STILL ALIVE (no clean
#       shutdown ran) — this is the leak the reaper exists to clean
#   04  start a fresh daemon → startup reap kills the orphan
#   05  no process carrying the orphan's --id survives
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built
# (make build), curl. Boots from an OCI image so the fresh embedded agent
# is used (no profile-rootfs rebuild needed).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_reap.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7888}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-alpine:latest}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-reap-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible orphan-VMM reap smoke"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " chroot base: $CHROOT_BASE"
echo " listen     : $LISTEN"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

# jailer_id <sandbox-id> — the jailer/firecracker --id is the sandbox id with
# underscores swapped for hyphens (see sanitizeJailerID).
jailer_id() { echo "${1//_/-}"; }

# vmm_alive <jailer-id> — true if any process carries "--id <jailer-id>".
# A process can exit between the glob and the read; group-suppress stderr so
# that benign race doesn't print "No such file or directory".
vmm_alive() {
  local id="$1" p
  for p in /proc/[0-9]*/cmdline; do
    { tr '\0' ' ' <"$p" | grep -q -- "--id $id "; } 2>/dev/null && return 0
  done
  return 1
}

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" \
    --network-egress-iface "$EGRESS_IFACE" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..100}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}

JID=""
final_cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null
  [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null
  # Belt-and-suspenders: if the test failed and left the orphan alive, kill it
  # so we never leak a firecracker out of the smoke.
  if [[ -n "$JID" ]] && vmm_alive "$JID"; then
    for p in /proc/[0-9]*/cmdline; do
      { tr '\0' ' ' <"$p" | grep -q -- "--id $JID "; } 2>/dev/null && kill -9 "$(basename "$(dirname "$p")")" 2>/dev/null
    done
  fi
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$SMOKE_ROOT"
}
trap final_cleanup EXIT

echo "== 01 starting daemon + booting a sandbox from $IMAGE"
start_daemon
SBX="$(cli sandbox create --image "$IMAGE" --memory 256)"
if [[ "$SBX" != sbx_* ]]; then
  fail "create --image $IMAGE failed: $SBX"; tail -30 "$DAEMON_LOG"; exit 3
fi
JID="$(jailer_id "$SBX")"
pass "booted $SBX (jailer id $JID)"

echo "== 02 the VMM is running"
if vmm_alive "$JID"; then
  pass "firecracker/jailer for $JID is alive"
else
  fail "no VMM process for $JID after create"; tail -30 "$DAEMON_LOG"; exit 1
fi

echo "== 03 kill -9 the daemon (no clean shutdown) — VMM should orphan"
kill -9 "$DAEMON_PID" 2>/dev/null
wait "$DAEMON_PID" 2>/dev/null
DAEMON_PID=""
sleep 1
if vmm_alive "$JID"; then
  pass "VMM survived the daemon kill (orphaned) — the leak the reaper targets"
else
  # If it died on its own the reap test is moot, but it's not a failure of reaping.
  echo "   NOTE: VMM already gone after daemon kill; reap has nothing to do"
fi

echo "== 04 restart the daemon → startup reap runs"
start_daemon
pass "fresh daemon healthy (reap ran at startup)"

echo "== 05 the orphan VMM is gone"
# Give the reap a moment (KillLiveOrphans waits for drain, but be generous).
gone=0
for _ in {1..25}; do
  if ! vmm_alive "$JID"; then gone=1; break; fi
  sleep 0.2
done
if [[ "$gone" == "1" ]]; then
  pass "no process carrying --id $JID survives the restart"
else
  fail "orphan VMM for $JID still alive after restart+reap"; grep -i "orphan\|reap" "$DAEMON_LOG" | tail
fi

echo "==============================================================="
echo " reap smoke: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
