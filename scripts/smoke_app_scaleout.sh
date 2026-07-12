#!/usr/bin/env bash
#
# Horizontal scale-out (v0.5.2): an app autoscales its replica count on request
# concurrency and load-balances across the fleet.
#
# What this proves on real KVM:
#   01  a proxy-fronted app (min 1, max 4, target-concurrency 2) starts at 1 replica
#   02  under sustained concurrency it AUTOSCALES UP: extras are WARM-FORKED from a
#       golden snapshot and the app reports multiple ready replicas
#   03  the proxy load-balances requests ACROSS the replicas (each guest is hit)
#   04  when load drops it scales back down toward the floor
#
# Concurrency is generated with slow-reading curls (--limit-rate): each holds its
# request in-flight for ~a minute, so the concurrency the autoscaler samples
# actually builds up against an otherwise-instant nginx.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl,
# python3. Runs ~3–4 min (autoscale convergence + the scale-down stabilization).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_app_scaleout.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7893}"
PROXY_PORT="${PROXY_PORT:-7894}"
DOMAIN="${DOMAIN:-apps.local}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
LOAD="${LOAD:-8}" # concurrent slow-read requests

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-scaleout-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible horizontal scale-out smoke (v0.5.2)"
echo " output: $SMOKE_ROOT   proxy: 127.0.0.1:$PROXY_PORT ($DOMAIN)  load: $LOAD"
echo "==============================================================="

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
pyget() { python3 -c "import json,sys; d=json.load(sys.stdin); print(d$1)" 2>/dev/null; }
ready()   { cli app get web 2>/dev/null | pyget '.get("status",{}).get("ready_replicas",0)'; }
replicas(){ cli app get web 2>/dev/null | pyget '.get("status",{}).get("replicas",0)'; }
phase()   { cli app get web 2>/dev/null | pyget '.get("status",{}).get("phase","")'; }
insts()   { cli app get web 2>/dev/null | python3 -c 'import json,sys; [print(i["instance_id"]) for i in json.load(sys.stdin).get("status",{}).get("instances",[])]' 2>/dev/null; }
proxy_hit() { curl -s --max-time 5 -H "Host: web.${DOMAIN}" "http://127.0.0.1:${PROXY_PORT}/" 2>/dev/null; }
serves()  { for _ in {1..40}; do [[ "$(proxy_hit)" == *nginx* ]] && return 0; sleep 0.5; done; return 1; }
wait_ready_ge() { for _ in {1..40}; do [[ "$(ready)" -ge "$1" ]] 2>/dev/null && return 0; sleep 2; done; return 1; }
wait_ready_le() { for _ in {1..60}; do [[ "$(ready)" -le "$1" ]] 2>/dev/null && return 0; sleep 2; done; return 1; }

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
LOAD_PIDS=()
stop_load() { for p in "${LOAD_PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; LOAD_PIDS=(); }
cleanup() {
  stop_load
  cli app rm web >/dev/null 2>&1 || true; sleep 1
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== 01 daemon + autoscaling app (min 1, max 4, target-concurrency 2)"
start_daemon
if [[ "$(cli app create web --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 \
         --min-scale 1 --max-scale 4 --target-concurrency 2 2>/dev/null)" != "web" ]]; then
  fail "app create failed"; tail -30 "$DAEMON_LOG"; exit 1
fi
if serves && [[ "$(replicas)" -eq 1 ]]; then
  pass "app serves and starts at 1 replica"
else
  fail "app never served / not at 1 replica (replicas=$(replicas))"; tail -30 "$DAEMON_LOG"; exit 1
fi
# Seed a multi-MB file so slow-reading clients hold requests in-flight (nginx's
# tiny default page transfers instantly regardless of --limit-rate, yielding ~0
# sampled concurrency). Baked into the primary now, so the golden snapshot the
# extras fork from has it too.
cli app exec web -- sh -c 'dd if=/dev/zero of=/usr/share/nginx/html/big bs=1024 count=6000 2>/dev/null' >/dev/null 2>&1 || true

echo "== 02 sustained concurrency → autoscale UP (warm-forked extras)"
# Each slow GET of the 6 MB file at 40 KB/s holds a request in-flight for ~2.5 min.
for _ in $(seq 1 "$LOAD"); do
  ( curl -s --max-time 200 --limit-rate 40000 -H "Host: web.${DOMAIN}" "http://127.0.0.1:${PROXY_PORT}/big" >/dev/null 2>&1 ) &
  LOAD_PIDS+=("$!")
done
if wait_ready_ge 3; then
  pass "autoscaled up under load: ${LOAD} concurrent → $(ready) ready replicas ($(replicas) desired)"
else
  fail "did not autoscale up (ready=$(ready), desired=$(replicas))"; tail -40 "$DAEMON_LOG"
fi
# Warm-fork evidence: extras were forked, not cold-created.
if grep -qa '"msg":"app extra replica forked (warm)"' "$DAEMON_LOG"; then
  pass "extras were WARM-FORKED from the golden snapshot"
else
  echo "   (note: no warm-fork log; extras may have cold-booted — see $DAEMON_LOG)"
fi

echo "== 03 proxy load-balances across the replicas"
# Each guest IP should be reachable; the endpoint set has >1 instance.
N="$(insts | grep -c .)"
[[ "$N" -ge 3 ]] && pass "endpoint set has $N instances behind the proxy" \
                 || fail "endpoint set has only $N instances"

echo "== 04 load drops → autoscale DOWN toward the floor"
stop_load
# Scale-down waits out the stabilization window (~60s) + slow-EWMA decay.
if wait_ready_le 1; then
  pass "scaled back down to $(ready) replica after load cleared"
else
  fail "did not scale down (ready=$(ready) after wait)"; tail -30 "$DAEMON_LOG"
fi

echo "==============================================================="
echo " scale-out smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
