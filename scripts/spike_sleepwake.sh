#!/usr/bin/env bash
#
# Smoke test for in-place sleep/wake (v0.5.0).
#
# Drives the real sandbox-level primitives: SleepInPlace snapshots + stops the
# VMM (freeing RAM) keeping the netns; WakeInPlace restores in place, reseeding
# the CRNG and stepping the guest clock via the agent /wake endpoint.
#
# What this proves on real KVM:
#   1. A networked, listening guest can be snapshotted, its VMM killed
#      (RAM freed), and then RESTORED IN PLACE — same id, same netns, same IP.
#   2. The netns is KEPT across sleep (teardown asymmetry): unlike Delete, sleep
#      must not tear down the network. We assert the netns count is unchanged.
#   3. The firecracker process is actually GONE while asleep (RAM reclaimed).
#   4. The guest's TCP listener survives snapshot→restore: the published
#      port serves again after wake, with no re-publish and no identity refresh.
#
# The sharp unknown this targets: reusing the SAME jailer chroot / sandbox id
# across the cycle (fork always allocates a fresh id, so this is unexercised).
# If wake fails, read the daemon log for a jailer "chroot exists / resource
# busy" error — that's finding #1 and tells us Sleep must also clean the chroot.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built
# (make build), curl, and internet (pulls nginx:alpine) OR a local nginx image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/spike_sleepwake.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7889}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
PUB_PORT="${PUB_PORT:-8088}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-spike-sleepwake-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible SPIKE: in-place sleep / wake"
echo " output dir : $SMOKE_ROOT     listen: $LISTEN"
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

fc_count()   { local n; n="$(pgrep -c firecracker 2>/dev/null)"; echo "${n:-0}"; }
netns_count(){ ip netns list 2>/dev/null | wc -l; }
hit() { # hit <url> <needle>
  local url="$1" needle="$2" body
  for _ in {1..30}; do
    body="$(curl -s --max-time 3 "$url" 2>/dev/null || true)"
    [[ "$body" == *"$needle"* ]] && return 0
    sleep 0.5
  done
  return 1
}

# ---- daemon -----------------------------------------------------------------
DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" \
    --network-egress-iface "$EGRESS_IFACE" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..50}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -20 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3
}
cleanup() {
  for id in $(curl -sf "$BASE_URL/sandboxes" 2>/dev/null |
    python3 -c 'import json,sys;[print(s["id"]) for s in json.load(sys.stdin)["sandboxes"]]' 2>/dev/null); do
    curl -sf -X DELETE "$BASE_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== 01 start daemon"
start_daemon; pass "daemon healthy"
NS_BASE="$(netns_count)"

echo "== 02 create a networked, listening sandbox ($IMAGE, publish $PUB_PORT:80)"
SBX="$(cli sandbox create --image "$IMAGE" --pull missing --memory 256 --publish "${PUB_PORT}:80")"
if [[ "$SBX" != sbx_* ]]; then fail "create failed: $SBX"; tail -30 "$DAEMON_LOG"; exit 1; fi
if hit "http://localhost:${PUB_PORT}/" "nginx"; then pass "guest serves before sleep ($SBX)"; else
  fail "guest never served before sleep"; tail -30 "$DAEMON_LOG"; exit 1; fi
FC_RUN="$(fc_count)"; NS_RUN="$(netns_count)"
echo "   firecracker procs running=$FC_RUN ; netns count=$NS_RUN (base=$NS_BASE)"

echo "== 03 SLEEP (snapshot → kill VMM, keep netns)"
if curl -sf -X POST "$BASE_URL/sandboxes/$SBX/sleep" >/dev/null; then pass "sleep returned 200"; else
  fail "sleep call failed"; tail -30 "$DAEMON_LOG"; exit 1; fi
sleep 1
FC_SLEEP="$(fc_count)"; NS_SLEEP="$(netns_count)"
echo "   firecracker procs asleep=$FC_SLEEP ; netns count=$NS_SLEEP"
[[ "$FC_SLEEP" -lt "$FC_RUN" ]] && pass "VMM gone while asleep (RAM freed: $FC_RUN→$FC_SLEEP)" \
                                || fail "VMM still running while asleep ($FC_SLEEP)"
[[ "$NS_SLEEP" -eq "$NS_RUN" ]] && pass "netns KEPT across sleep (teardown asymmetry ok: $NS_SLEEP)" \
                                || fail "netns changed across sleep ($NS_RUN→$NS_SLEEP) — sleep tore down the network"
curl -s --max-time 3 "http://localhost:${PUB_PORT}/" >/dev/null 2>&1 \
  && echo "   (note: port still answered while asleep — unexpected but non-fatal)" \
  || echo "   (expected: published port does not serve while asleep)"

echo "== 04 WAKE (restore in place — same id/netns/IP)"
if curl -sf -X POST "$BASE_URL/sandboxes/$SBX/wake" >/dev/null; then pass "wake returned 200"; else
  fail "wake call failed — CHECK DAEMON LOG for a jailer chroot/id-reuse error (finding #1)"; tail -40 "$DAEMON_LOG"; exit 1; fi
if hit "http://localhost:${PUB_PORT}/" "nginx"; then
  pass "guest SERVES AGAIN after wake — listener survived, same IP, forwarder intact"
else
  fail "guest did not serve after wake"; tail -40 "$DAEMON_LOG"; exit 1; fi
FC_WAKE="$(fc_count)"; NS_WAKE="$(netns_count)"
echo "   firecracker procs after wake=$FC_WAKE ; netns count=$NS_WAKE"

echo "==============================================================="
echo " spike sleep/wake: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
