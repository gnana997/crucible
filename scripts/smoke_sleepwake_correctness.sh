#!/usr/bin/env bash
#
# Correctness matrix for app sleep/wake (v0.5.0 M1, Group D). Where
# smoke_app_sleepwake.sh proves scale-to-zero *works*, this proves it is
# *correct* — the two silent-corruption gates the functional smoke can't see:
#
#   D2 / E4  guest clock is STEPPED on wake. After a real sleep gap, an asleep
#            guest's clock is frozen at snapshot time; wake steps it to host
#            wall time. We sleep the app for GAP seconds, wake it, and assert the
#            guest's clock is within a few seconds of the host's — NOT ~GAP
#            behind (which is what a frozen, un-stepped clock would show).
#
#   D3 / E1  identity is PRESERVED across wake (wake != fork). Wake reseeds the
#            CRNG but must NOT rotate hostname/machine-id. We capture the guest
#            hostname before sleep and assert it is unchanged after wake.
#
# D1/E2 (a guest TCP listener survives restore) and D4 (the slept instance's
# netns is not reaped) are already demonstrated by smoke_app_sleepwake.sh: the
# app serves again on wake (listener survived) at the same guest IP (netns kept).
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl,
# python3, and internet (pulls nginx:alpine) or a cached image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_sleepwake_correctness.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7893}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
GAP="${GAP:-15}"          # seconds asleep — must exceed CLOCK_TOL to be meaningful
CLOCK_TOL="${CLOCK_TOL:-5}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-sw-correctness-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible sleep/wake correctness (v0.5.0 M1, Group D)"
echo " output dir : $SMOKE_ROOT   gap: ${GAP}s   clock tol: ${CLOCK_TOL}s"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null    || { echo "error: curl needed" >&2; exit 2; }
command -v python3 >/dev/null || { echo "error: python3 needed" >&2; exit 2; }
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }
phase() { cli app get web 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
# gexec <cmd...> — run a command in the app's current instance, return trimmed stdout.
gexec() { cli app exec web -- "$@" 2>/dev/null | tr -d '\r' | tail -1 | tr -d '[:space:]'; }
wait_phase() { for _ in {1..60}; do [[ "$(phase)" == "$1" ]] && return 0; sleep 0.5; done; return 1; }

# ---- daemon + app -----------------------------------------------------------
DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --network-egress-iface "$EGRESS_IFACE" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}
cleanup() {
  cli app rm web >/dev/null 2>&1 || true; sleep 1
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== 01 start daemon + create app"
start_daemon
if [[ "$(cli app create web --image "$IMAGE" --pull missing --restart always --memory 256 2>/dev/null)" != "web" ]]; then
  fail "app create failed"; tail -30 "$DAEMON_LOG"; exit 1
fi
if ! wait_phase running; then fail "app never became running"; tail -30 "$DAEMON_LOG"; exit 1; fi
# The guest must answer exec (has a shell + coreutils) for the probes below.
HOST_BEFORE="$(gexec hostname)"
if [[ -z "$HOST_BEFORE" ]]; then fail "guest exec returned no hostname"; tail -30 "$DAEMON_LOG"; exit 1; fi
pass "app running; guest hostname=$HOST_BEFORE"

echo "== 02 sleep for ${GAP}s (frozen guest clock accrues drift)"
cli app sleep web >/dev/null 2>&1
if [[ "$(phase)" != "asleep" ]]; then fail "phase=$(phase), want asleep"; exit 1; fi
sleep "$GAP"

echo "== 03 wake"
cli app wake web >/dev/null 2>&1
if ! wait_phase running; then fail "app did not return to running after wake"; tail -40 "$DAEMON_LOG"; exit 1; fi
pass "woke to running"

echo "== 04 D3/E1: identity preserved (hostname unchanged, wake != fork)"
HOST_AFTER="$(gexec hostname)"
if [[ "$HOST_AFTER" == "$HOST_BEFORE" ]]; then
  pass "hostname unchanged across sleep/wake ($HOST_AFTER)"
else
  fail "hostname changed: before=$HOST_BEFORE after=$HOST_AFTER (wake must not rotate identity)"
fi

echo "== 05 D2/E4: guest clock stepped to host time (not ~${GAP}s behind)"
HOST_NOW="$(date +%s)"
GUEST_NOW="$(gexec date +%s)"
if [[ "$GUEST_NOW" =~ ^[0-9]+$ ]]; then
  DIFF=$(( HOST_NOW > GUEST_NOW ? HOST_NOW - GUEST_NOW : GUEST_NOW - HOST_NOW ))
  echo "   host=$HOST_NOW guest=$GUEST_NOW diff=${DIFF}s (tol ${CLOCK_TOL}s; frozen clock would be ~${GAP}s)"
  if [[ "$DIFF" -le "$CLOCK_TOL" ]]; then
    pass "guest clock stepped on wake (drift ${DIFF}s <= ${CLOCK_TOL}s)"
  else
    fail "guest clock NOT stepped: ${DIFF}s drift (E4 gate — stale clock breaks TLS/JWT/TTLs)"
  fi
else
  fail "could not read guest clock: '$GUEST_NOW'"
fi

echo "==============================================================="
echo " sleep/wake correctness: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
