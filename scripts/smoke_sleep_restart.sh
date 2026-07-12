#!/usr/bin/env bash
#
# A slept app survives a daemon restart (v0.5.0).
#
# What this proves on real KVM:
#   01  a proxy-fronted app is created and served by name
#   02  `app sleep web` captures a DURABLE snapshot (RAM freed)
#   03  the daemon is STOPPED and STARTED again over the same state dirs; the
#       snapshot survives, and the app is RE-ADOPTED as asleep (not cold-booted
#       — no firecracker process runs after restart)
#   04  a request through the proxy WAKES it from the durable snapshot (a fresh
#       instance, restored warm state) and serves it
#
# This is the "free durability" that beats crucible's old behavior, where no
# live workload survived a daemon restart at all.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl,
# python3, internet (pulls nginx:alpine) or a cached image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_sleep_restart.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7897}"
PROXY_PORT="${PROXY_PORT:-7898}"
DOMAIN="${DOMAIN:-apps.local}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-sleep-restart-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible sleep-survives-restart smoke (v0.5.0)"
echo " output: $SMOKE_ROOT   proxy: http://127.0.0.1:$PROXY_PORT ($DOMAIN)"
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
phase() { cli app get web 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
snap_dirs() { ls -1d "$WORK_BASE"/snap_* 2>/dev/null | wc -l; }
proxy_hit() { curl -s --max-time 8 -H "Host: web.${DOMAIN}" "http://127.0.0.1:${PROXY_PORT}/" 2>/dev/null; }
serves() { for _ in {1..40}; do [[ "$(proxy_hit)" == *nginx* ]] && return 0; sleep 0.5; done; return 1; }
wait_phase() { for _ in {1..80}; do [[ "$(phase)" == "$1" ]] && return 0; sleep 0.5; done; return 1; }

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --network-egress-iface "$EGRESS_IFACE" \
    --proxy-listen "127.0.0.1:$PROXY_PORT" --proxy-domain "$DOMAIN" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}
stop_daemon() {
  [[ -n "$DAEMON_PID" ]] || return 0
  kill -TERM "$DAEMON_PID" 2>/dev/null
  for _ in {1..100}; do kill -0 "$DAEMON_PID" 2>/dev/null || break; sleep 0.2; done
  wait "$DAEMON_PID" 2>/dev/null; DAEMON_PID=""
}
cleanup() { stop_daemon; }
trap cleanup EXIT

echo "== 01 daemon + proxy-fronted app"
start_daemon
if [[ "$(cli app create web --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 2>/dev/null)" != "web" ]]; then
  fail "app create failed"; tail -30 "$DAEMON_LOG"; exit 1
fi
if serves && wait_phase running; then pass "app serves by name through the proxy"; else
  fail "app never served"; tail -30 "$DAEMON_LOG"; exit 1; fi

echo "== 02 sleep (durable snapshot)"
cli app sleep web >/dev/null 2>&1
if [[ "$(phase)" == "asleep" ]] && [[ "$(snap_dirs)" -ge 1 ]]; then
  pass "asleep with a durable snapshot on disk ($(snap_dirs) snap dir)"
else
  fail "sleep did not produce an asleep app + snapshot (phase=$(phase), snaps=$(snap_dirs))"; exit 1
fi

echo "== 03 RESTART the daemon; app is re-adopted asleep (not cold-booted)"
stop_daemon
sleep 1
[[ "$(fc_count)" -eq 0 ]] && pass "no firecracker running after daemon stop" || fail "firecracker still running after stop ($(fc_count))"
[[ "$(snap_dirs)" -ge 1 ]] && pass "durable snapshot survived the daemon stop" || fail "snapshot lost across restart"
start_daemon
if wait_phase asleep; then
  sleep 1
  [[ "$(fc_count)" -eq 0 ]] && pass "re-adopted as asleep, no VMM cold-booted" \
                            || fail "a VMM was booted (cold-boot instead of re-adopt): fc=$(fc_count)"
else
  fail "app not re-adopted asleep after restart (phase=$(phase))"; tail -40 "$DAEMON_LOG"; exit 1
fi

echo "== 04 a request wakes it from the durable snapshot"
if [[ "$(proxy_hit)" == *nginx* ]] && wait_phase running; then
  pass "request woke the app from snapshot + served (phase running, fc=$(fc_count))"
else
  fail "post-restart wake did not serve"; tail -40 "$DAEMON_LOG"; exit 1
fi

# The app is running again; app rm should GC the (now spent) snapshot.
cli app rm web >/dev/null 2>&1; sleep 2

echo "==============================================================="
echo " sleep-restart smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
