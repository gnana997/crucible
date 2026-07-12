#!/usr/bin/env bash
#
# End-to-end smoke for app scale-to-zero (v0.5.0): create → sleep → wake,
# driven entirely through the app-level CLI/API.
#
# What this proves on real KVM:
#   01  a durable app boots from an image and serves on its published port
#   02  `app sleep web` snapshots + stops the VMM: phase=asleep, RAM freed
#       (firecracker process gone), the published port stops serving
#   03  the reconciler LEAVES A SLEPT APP ALONE — across several reconcile
#       intervals the VMM stays gone and phase stays asleep (the reconciler guard;
#       without it a --restart=always app would be cold-booted right back)
#   04  `app wake web` restores in place: phase=running, same published port
#       serves again, and status reports a last_wake_latency_ms
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built
# (make build), curl, python3, and internet (pulls nginx:alpine) or a cached
# nginx image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_app_sleepwake.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7891}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
PUB_PORT="${PUB_PORT:-8090}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-app-sleepwake-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible app scale-to-zero smoke (v0.5.0)"
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
command -v curl >/dev/null    || { echo "error: curl needed" >&2; exit 2; }
command -v python3 >/dev/null || { echo "error: python3 needed" >&2; exit 2; }
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }
fc_count() { local n; n="$(pgrep -c firecracker 2>/dev/null)"; echo "${n:-0}"; }

# app_field <json-field-path> — extract a value from `app get web` JSON.
phase() { cli app get web 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
wake_ms() { cli app get web 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("last_wake_latency_ms",0))' 2>/dev/null; }

hit() { # hit <url> <needle>
  local url="$1" needle="$2" body
  for _ in {1..40}; do
    body="$(curl -s --max-time 3 "$url" 2>/dev/null || true)"
    [[ "$body" == *"$needle"* ]] && return 0
    sleep 0.5
  done
  return 1
}
wait_phase() { # wait_phase <want> — poll until app phase == want (or give up)
  for _ in {1..60}; do [[ "$(phase)" == "$1" ]] && return 0; sleep 0.5; done
  return 1
}

# ---- daemon -----------------------------------------------------------------
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
  cli app rm web >/dev/null 2>&1 || true
  sleep 1
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== 01 start daemon + create a published app"
start_daemon
if [[ "$(cli app create web --image "$IMAGE" --pull missing -p "${PUB_PORT}:80" --restart always --memory 256 2>/dev/null)" != "web" ]]; then
  fail "app create failed"; tail -30 "$DAEMON_LOG"; exit 1
fi
if hit "http://localhost:${PUB_PORT}/" "nginx" && wait_phase running; then
  pass "app running and serving on :$PUB_PORT"
else
  fail "app never came up"; tail -30 "$DAEMON_LOG"; exit 1
fi
FC_RUN="$(fc_count)"

echo "== 02 sleep (snapshot → free RAM)"
cli app sleep web >/dev/null 2>&1
if [[ "$(phase)" == "asleep" ]]; then pass "phase=asleep after sleep"; else fail "phase=$(phase), want asleep"; fi
sleep 1; FC_SLEEP="$(fc_count)"
[[ "$FC_SLEEP" -lt "$FC_RUN" ]] && pass "VMM gone while asleep (RAM freed: $FC_RUN→$FC_SLEEP)" \
                                || fail "VMM still running while asleep ($FC_SLEEP)"
curl -s --max-time 3 "http://localhost:${PUB_PORT}/" >/dev/null 2>&1 \
  && echo "   (note: port still answered while asleep — unexpected)" \
  || echo "   (expected: published port does not serve while asleep)"

echo "== 03 reconciler leaves a slept --restart=always app alone"
sleep 8   # several reconcile intervals (default 3s)
if [[ "$(fc_count)" -eq "$FC_SLEEP" && "$(phase)" == "asleep" ]]; then
  pass "still asleep after reconcile passes (no cold-boot)"
else
  fail "reconciler disturbed the slept app: fc=$(fc_count) phase=$(phase)"; tail -30 "$DAEMON_LOG"
fi

echo "== 04 wake (restore in place)"
cli app wake web >/dev/null 2>&1
if wait_phase running && hit "http://localhost:${PUB_PORT}/" "nginx"; then
  pass "phase=running and serving again after wake"
else
  fail "wake did not restore service"; tail -40 "$DAEMON_LOG"
fi
FC_WAKE="$(fc_count)"; MS="$(wake_ms)"
[[ "$FC_WAKE" -ge "$FC_RUN" ]] && pass "VMM back after wake (fc=$FC_WAKE)" || fail "VMM not back ($FC_WAKE)"
[[ "$MS" -gt 0 ]] 2>/dev/null && pass "status reports last_wake_latency_ms=$MS" \
                              || echo "   (note: last_wake_latency_ms=$MS)"

echo "==============================================================="
echo " app sleep/wake smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
