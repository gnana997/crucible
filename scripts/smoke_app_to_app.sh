#!/usr/bin/env bash
#
# App→app service networking (v0.5.1): apps reach each other by name over the
# ingress proxy VIP, default-deny, with scale-to-zero on internal calls.
#
# What this proves on real KVM:
#   01  three proxy-fronted apps come up: backend, secret, and caller
#       (caller is granted --can-call backend, NOT secret)
#   02  GRANTED: from caller's guest, `wget http://backend.internal/` is served
#   03  DEFAULT-DENY: from caller's guest, `wget http://secret.internal/` fails
#       (caller has no grant → DNS NXDOMAIN / proxy 403 — no inventory leak)
#   04  SCALE-TO-ZERO on an internal call: backend auto-sleeps when idle, and the
#       next internal call from caller transparently wakes + serves it
#   05  PEER ISOLATION intact: caller's guest CANNOT reach backend's guest IP
#       directly (the VIP is the only path — app→app didn't open lateral access)
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl,
# python3, and internet (pulls nginx:alpine) or a cached image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_app_to_app.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7897}"
PROXY_PORT="${PROXY_PORT:-7898}"
INTERNAL_PORT="${INTERNAL_PORT:-80}" # the app→app VIP port (backend.internal:80)
DOMAIN="${DOMAIN:-apps.local}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
IDLE="${IDLE:-10s}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-app2app-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible app→app networking smoke (v0.5.1)"
echo " output: $SMOKE_ROOT   proxy: 127.0.0.1:$PROXY_PORT   VIP :$INTERNAL_PORT"
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
api()  { curl -s --max-time 5 "$@"; }
pyget() { python3 -c "import json,sys; d=json.load(sys.stdin); print(d$1)" 2>/dev/null; }
phase()    { cli app get "$1" 2>/dev/null | pyget '.get("status",{}).get("phase","")'; }
app_inst() { cli app get "$1" 2>/dev/null | pyget '.get("status",{}).get("instance_id","")'; }
guest_ip() { api "$BASE_URL/sandboxes/$1" 2>/dev/null | grep -o '"guest_ip":"[0-9.]*"' | grep -o '[0-9.]*' | head -1; }
# Run a command inside an app's current instance, capturing stdout.
exec_in()  { local app="$1"; shift; cli app exec "$app" -- "$@" 2>/dev/null; }
wait_phase() { for _ in {1..80}; do [[ "$(phase "$1")" == "$2" ]] && return 0; sleep 0.5; done; return 1; }

# ---- daemon (proxy + app→app networking enabled) ----------------------------
DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --network-egress-iface "$EGRESS_IFACE" \
    --proxy-listen "127.0.0.1:$PROXY_PORT" --proxy-domain "$DOMAIN" \
    --internal-networking --internal-proxy-port "$INTERNAL_PORT" \
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
  for a in caller backend secret; do cli app rm "$a" >/dev/null 2>&1 || true; done
  sleep 1
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== 01 daemon + three apps (caller --can-call backend, NOT secret)"
start_daemon
create_ok=1
cli app create backend --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 --idle-timeout "$IDLE" >/dev/null 2>&1 || create_ok=0
cli app create secret  --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 >/dev/null 2>&1 || create_ok=0
cli app create caller  --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 --can-call backend >/dev/null 2>&1 || create_ok=0
[[ "$create_ok" -eq 1 ]] || { fail "app create failed"; tail -40 "$DAEMON_LOG"; exit 1; }
if wait_phase backend running && wait_phase secret running && wait_phase caller running; then
  pass "backend, secret, caller all running"
else
  fail "an app never reached running"; tail -40 "$DAEMON_LOG"; exit 1
fi
# Confirm the caller's spec carries the grant.
cli app get caller | grep -q '"can_call"' && pass "caller can_call surfaced in app get" || echo "   (note: can_call not in app get output)"

echo "== 02 GRANTED: caller → http://backend.internal/ is served"
GOT="$(exec_in caller wget -T 5 -q -O - "http://backend.internal:${INTERNAL_PORT}/")"
if [[ "$GOT" == *nginx* || "$GOT" == *"<html"* ]]; then
  pass "caller reached backend.internal (served $(echo "$GOT" | wc -c) bytes)"
else
  fail "caller could NOT reach granted backend.internal (got: '${GOT:0:60}')"; tail -40 "$DAEMON_LOG"
fi

echo "== 03 DEFAULT-DENY: caller → http://secret.internal/ is refused"
DENIED="$(exec_in caller wget -T 5 -q -O - "http://secret.internal:${INTERNAL_PORT}/")"
if [[ -z "$DENIED" || ( "$DENIED" != *nginx* && "$DENIED" != *"<html"* ) ]]; then
  pass "un-granted caller→secret.internal refused (no body served — default-deny holds)"
else
  fail "SECURITY: un-granted caller reached secret.internal! (got: '${DENIED:0:60}')"; tail -40 "$DAEMON_LOG"
fi

echo "== 04 SCALE-TO-ZERO: backend auto-sleeps, an internal call wakes it"
if wait_phase backend asleep; then
  pass "backend auto-slept while idle (phase asleep)"
  WOKE="$(exec_in caller wget -T 10 -q -O - "http://backend.internal:${INTERNAL_PORT}/")"
  if [[ ( "$WOKE" == *nginx* || "$WOKE" == *"<html"* ) ]] && wait_phase backend running; then
    pass "caller's internal call woke backend + was served (phase running)"
    MS="$(cli app get backend | pyget '.get("status",{}).get("last_wake_latency_ms",0)')"
    [[ "${MS:-0}" -gt 0 ]] 2>/dev/null && echo "   (last_wake_latency_ms=$MS)"
  else
    fail "internal call did not wake+serve backend"; tail -40 "$DAEMON_LOG"
  fi
else
  fail "backend did not auto-sleep within the window"; tail -40 "$DAEMON_LOG"
fi

echo "== 05 PEER ISOLATION: caller cannot reach backend's guest IP directly"
BINST="$(app_inst backend)"; BIP="$(guest_ip "$BINST")"
if [[ -n "$BIP" ]]; then
  # A direct guest→guest connection must be dropped (only the VIP path is open).
  if exec_in caller sh -c "nc -w 3 $BIP 80 </dev/null" >/dev/null 2>&1; then
    fail "SECURITY: caller reached backend's guest IP $BIP directly — lateral isolation broken!"
  else
    pass "caller cannot reach backend guest IP $BIP directly (VIP is the only path)"
  fi
else
  fail "could not read backend guest IP (inst=$BINST)"
fi

# Informational: the internal-request metric should have advanced.
IREQ="$(api "$BASE_URL/metrics" 2>/dev/null | awk '/^app_internal_requests_total/ {print $2}')"
echo "   (app_internal_requests_total=${IREQ:-n/a})"

echo "==============================================================="
echo " app→app networking smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
