#!/usr/bin/env bash
#
# App→app L4 networking (v0.9.5): a peer reaches an app's RAW TCP port at
# <app>.internal:PORT through a per-app internal VIP — any protocol, not just HTTP.
# Uses redis (RESP, a non-HTTP wire protocol) as the exposed service to prove the
# splice is a blind byte pipe.
#
# What this proves on real KVM:
#   01  a redis app exposes 6379 app→app (--internal-port 6379); its per-app VIP is
#       assigned and surfaced in `app get` (internal_vip)
#   02  GRANTED raw TCP: from the client's guest, `PING\r\n` to redis.internal:6379
#       returns `+PONG` — a non-HTTP protocol crosses the L4 splice end to end
#   03  DEFAULT-DENY: the client cannot reach a redis it was NOT --can-call'd to
#   04  ONLY DECLARED PORTS: the client cannot reach the VIP on an UNdeclared port
#       (proves nft opens just the exposed ports — no host 0.0.0.0 service via a VIP)
#   05  WAKE-ON-CONNECT: the redis app auto-sleeps when idle, and the next raw-TCP
#       connection transparently wakes + serves it
#   06  PEER ISOLATION intact: the client CANNOT reach redis's guest IP directly
#       (the VIP is the only path — L4 app→app didn't open lateral access)
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl,
# python3, and internet (pulls redis:alpine + nginx:alpine) or cached images.
# The guest images provide busybox `nc` (used to speak raw RESP).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_internal_l4.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-$KERNEL}" # apps boot from converted OCI images; base rootfs unused
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7899}"
PROXY_PORT="${PROXY_PORT:-7900}"
DOMAIN="${DOMAIN:-apps.local}"
VIP_CIDR="${VIP_CIDR:-10.21.0.0/16}"
BASE_URL="http://${LISTEN}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:alpine}"
CLIENT_IMAGE="${CLIENT_IMAGE:-nginx:alpine}"
IDLE="${IDLE:-10s}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-l4-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible app→app L4 networking smoke (v0.9.5)"
echo " output: $SMOKE_ROOT   proxy: 127.0.0.1:$PROXY_PORT   vip-cidr: $VIP_CIDR"
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
app_vip()  { cli app get "$1" 2>/dev/null | pyget '.get("internal_vip","")'; }
guest_ip() { api "$BASE_URL/sandboxes/$1" 2>/dev/null | grep -o '"guest_ip":"[0-9.]*"' | grep -o '[0-9.]*' | head -1; }
exec_in()  { local app="$1"; shift; cli app exec "$app" -- "$@" 2>/dev/null; }
# Speak raw RESP PING to host:port from an app's guest; echoes the reply text.
resp_ping() { exec_in "$1" sh -c "printf 'PING\r\n' | nc -w 3 $2 $3"; }
wait_phase() { for _ in {1..80}; do [[ "$(phase "$1")" == "$2" ]] && return 0; sleep 0.5; done; return 1; }

# ---- daemon (proxy + internal-networking + L4) ------------------------------
DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$ROOTFS" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --network-egress-iface "$EGRESS_IFACE" \
    --proxy-listen "127.0.0.1:$PROXY_PORT" --proxy-domain "$DOMAIN" \
    --internal-networking --internal-l4 --internal-vip-cidr "$VIP_CIDR" \
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
  for a in client redis stranger; do cli app rm "$a" >/dev/null 2>&1 || true; done
  sleep 1
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== 01 daemon + apps: redis (exposes 6379), client (--can-call redis), stranger (not granted)"
start_daemon
create_ok=1
# redis MUST bind all interfaces (its default is 127.0.0.1, unreachable from the
# proxy's dial to the guest IP) — override the entrypoint. The tcp:6379 health check
# dials the guest IP, so "running" proves the port is actually reachable app→app.
# It exposes 6379 app→app and auto-sleeps when idle (for the wake test).
REDIS_CMD=(redis-server --bind 0.0.0.0 --protected-mode no)
cli app create redis    --image "$REDIS_IMAGE"  --pull missing --restart always --memory 256 \
    --internal-port 6379 --health "tcp:6379" --idle-timeout "$IDLE" \
    -- "${REDIS_CMD[@]}" >/dev/null 2>&1 || create_ok=0
# stranger also exposes redis, but the client is NOT granted --can-call to it.
cli app create stranger --image "$REDIS_IMAGE"  --pull missing --restart always --memory 256 \
    --internal-port 6379 --health "tcp:6379" \
    -- "${REDIS_CMD[@]}" >/dev/null 2>&1 || create_ok=0
cli app create client   --image "$CLIENT_IMAGE" --pull missing --restart always --memory 256 \
    --port 80 --can-call redis >/dev/null 2>&1 || create_ok=0
[[ "$create_ok" -eq 1 ]] || { fail "app create failed"; tail -40 "$DAEMON_LOG"; exit 1; }
if wait_phase redis running && wait_phase stranger running && wait_phase client running; then
  pass "redis, stranger, client all running"
else
  fail "an app never reached running"; tail -40 "$DAEMON_LOG"; exit 1
fi
VIP="$(app_vip redis)"
if [[ -n "$VIP" ]]; then
  pass "redis assigned a per-app internal VIP ($VIP)"
else
  fail "redis has no internal_vip in app get (VIP not assigned/surfaced)"; tail -40 "$DAEMON_LOG"
fi

echo "== 02 GRANTED raw TCP: client → redis.internal:6379 PING returns +PONG"
# Diagnostic: what does redis.internal resolve to from the client guest? (busybox
# ping prints the resolved IP on line 1 even if the ICMP itself is dropped.)
RESOLVED="$(exec_in client ping -w1 -c1 redis.internal 2>&1 | head -1)"
echo "   (client resolves 'redis.internal' → ${RESOLVED:-<none>} ; expected VIP $VIP)"
GOT="$(resp_ping client redis.internal 6379)"
if [[ "$GOT" == *PONG* ]]; then
  pass "client spoke RESP to redis.internal over the L4 splice (reply: $(echo "$GOT" | tr -d '\r\n'))"
else
  fail "client could NOT PING granted redis.internal:6379 (got: '${GOT:0:40}')"
  echo "   L4 metrics at failure:"; api "$BASE_URL/metrics" 2>/dev/null | awk '/^app_internal_l4/ {print "     " $0}'
  tail -40 "$DAEMON_LOG"
fi

echo "== 03 DEFAULT-DENY: client → stranger.internal:6379 refused (no --can-call)"
DENIED="$(resp_ping client stranger.internal 6379)"
if [[ "$DENIED" != *PONG* ]]; then
  pass "un-granted client→stranger.internal refused (no PONG — default-deny holds)"
else
  fail "SECURITY: un-granted client reached stranger.internal! (got: '${DENIED:0:40}')"; tail -40 "$DAEMON_LOG"
fi

echo "== 04 ONLY DECLARED PORTS: client → redis.internal:6380 (undeclared) refused"
UNDECL="$(resp_ping client redis.internal 6380)"
if [[ "$UNDECL" != *PONG* ]]; then
  pass "client cannot reach redis VIP on an undeclared port (nft opens only exposed ports)"
else
  fail "SECURITY: client reached an UNDECLARED VIP port! (got: '${UNDECL:0:40}')"; tail -40 "$DAEMON_LOG"
fi

echo "== 05 WAKE-ON-CONNECT: redis auto-sleeps, a raw-TCP connect wakes it"
if wait_phase redis asleep; then
  pass "redis auto-slept while idle (phase asleep)"
  WOKE="$(resp_ping client redis.internal 6379)"
  if [[ "$WOKE" == *PONG* ]] && wait_phase redis running; then
    pass "client's raw-TCP connect woke redis + was served (phase running)"
    MS="$(cli app get redis | pyget '.get("status",{}).get("last_wake_latency_ms",0)')"
    [[ "${MS:-0}" -gt 0 ]] 2>/dev/null && echo "   (last_wake_latency_ms=$MS)"
  else
    fail "raw-TCP connect did not wake+serve redis"; tail -40 "$DAEMON_LOG"
  fi
else
  fail "redis did not auto-sleep within the window"; tail -40 "$DAEMON_LOG"
fi

echo "== 06 PEER ISOLATION: client cannot reach redis's guest IP directly"
RINST="$(app_inst redis)"; RIP="$(guest_ip "$RINST")"
if [[ -n "$RIP" ]]; then
  DIRECT="$(exec_in client sh -c "printf 'PING\r\n' | nc -w 3 $RIP 6379")"
  if [[ "$DIRECT" == *PONG* ]]; then
    fail "SECURITY: client reached redis guest IP $RIP directly — lateral isolation broken!"
  else
    pass "client cannot reach redis guest IP $RIP directly (VIP is the only path)"
  fi
else
  fail "could not read redis guest IP (inst=$RINST)"
fi

# Informational: the L4 connection metric should show spliced + denied outcomes.
echo "   L4 metrics:"
api "$BASE_URL/metrics" 2>/dev/null | awk '/^app_internal_l4_(connections|bytes)_total/ {print "     " $0}'

echo "==============================================================="
echo " app→app L4 networking smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
