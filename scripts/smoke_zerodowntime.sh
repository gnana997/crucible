#!/usr/bin/env bash
#
# Zero-downtime rolling `app update` smoke (v0.4.3).
#
#   01  daemon healthy with the proxy enabled; a proxy-fronted app serves by name
#   02  hammer the app by name through the proxy while `app update` rolls it —
#       assert ZERO non-200 responses across the cutover, a NEW instance id, and
#       a bumped generation (boot new → ready → flip route → drain → destroy old)
#   03  the superseded instance is reaped after the drain window (back to one)
#
# The reconciler boots the new instance without flipping, waits for its readiness
# gate (health check, or a TCP connect to the app's port when none is set), flips
# the proxy route, then keeps the old instance alive for the drain window so the
# ~1s of stale routes + in-flight requests land on a live instance.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built with an
# embedded agent (make build), curl, network to pull the image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_zerodowntime.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7895}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
PROXY_PORT="${PROXY_PORT:-8889}"
DOMAIN="${DOMAIN:-apps.local}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-zdt-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"; LOG_DIR="$SMOKE_ROOT/logs"
APP_DB="$SMOKE_ROOT/apps.db"; DAEMON_LOG="$SMOKE_ROOT/daemon.log"
HAMMER_OUT="$SMOKE_ROOT/hammer.out"; HAMMER_STOP="$SMOKE_ROOT/hammer.stop"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible zero-downtime rolling-update smoke (v0.4.3)"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " proxy      : http://127.0.0.1:$PROXY_PORT (domain: $DOMAIN)"
echo "==============================================================="

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }; done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]] || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }

# The daemon must be new enough to have the rolling update (v0.4.3): the proxy
# (v0.4.2) is the prerequisite and the zero-downtime path builds on it.
if ! LC_ALL=C grep -qa "rolling update started\|proxy-listen" "$CRUCIBLE_BIN"; then
  echo "error: $CRUCIBLE_BIN predates zero-downtime rolling update (v0.4.3). Rebuild: make build" >&2
  exit 2
fi

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
api()  { curl -s "$@"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

appinst() { # print app web's current instance id (sbx_...) or empty
  api "$BASE_URL/apps/web" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1
}
appgen() { # print app web's generation
  api "$BASE_URL/apps/web" 2>/dev/null | grep -o '"generation":[0-9]*' | grep -o '[0-9]*' | head -1
}
hitproxy() { # <needle> — curl web by name until the body matches (or give up)
  local needle="$1" body
  for _ in $(seq 1 40); do
    body="$(curl -s --max-time 3 -H "Host: web.$DOMAIN" "http://127.0.0.1:$PROXY_PORT/" 2>/dev/null || true)"
    [[ "$body" == *"$needle"* ]] && return 0
    sleep 0.5
  done
  return 1
}

# hammer continuously curls the app by name through the proxy, recording one HTTP
# status per line, until the stop-file appears. A connect failure records 000.
hammer() {
  : > "$HAMMER_OUT"
  while [[ ! -f "$HAMMER_STOP" ]]; do
    curl -s -o /dev/null -w '%{http_code}\n' --max-time 4 \
      -H "Host: web.$DOMAIN" "http://127.0.0.1:$PROXY_PORT/" >> "$HAMMER_OUT" 2>/dev/null \
      || echo "000" >> "$HAMMER_OUT"
    sleep 0.05
  done
}

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
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
cleanup() {
  touch "$HAMMER_STOP" 2>/dev/null || true
  cli app rm web >/dev/null 2>&1 || true
  for id in $(api "$BASE_URL/sandboxes" 2>/dev/null | grep -o 'sbx_[a-z0-9]*' | sort -u); do
    api -X DELETE "$BASE_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null
  [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$SMOKE_ROOT"
}
trap cleanup EXIT

echo "== 01 starting daemon (proxy enabled) + a proxy-fronted app"
start_daemon
grep -qa '"msg":"ingress proxy enabled"' "$DAEMON_LOG" \
  && pass "daemon healthy, ingress proxy enabled" \
  || { fail "proxy did not enable (see $DAEMON_LOG)"; tail -20 "$DAEMON_LOG"; }

cli app rm web >/dev/null 2>&1 || true
# No --health: the update's readiness gate is a TCP connect to --port.
if [[ "$(cli app create web --image "$IMAGE" --port 80 --restart always --memory 256 2>/dev/null)" == "web" ]]; then
  hitproxy "html" || hitproxy "nginx" \
    && pass "app 'web' reachable by name through the proxy" \
    || { fail "app never served through the proxy"; tail -30 "$DAEMON_LOG"; }
else
  fail "app create web failed"; tail -30 "$DAEMON_LOG"
fi

# ---- 02 zero-downtime update: no dropped requests across the cutover ---------
echo "== 02 hammer web by name while 'app update' rolls it — expect zero drops"
INST1="$(appinst)"; GEN1="$(appgen)"
if [[ "$INST1" != sbx_* ]]; then
  fail "could not read web's instance id before the update"
else
  hammer & HPID=$!
  sleep 1  # establish a baseline of 200s before the update

  # A config change (memory + an env var) bumps the generation → rolling redeploy.
  cli app update web --image "$IMAGE" --port 80 --restart always --memory 320 -e ROLL=v2 >/dev/null 2>&1

  # Wait for the flip: the current instance id changes to a new one.
  INST2="";
  for _ in $(seq 1 120); do
    INST2="$(appinst)"
    [[ "$INST2" == sbx_* && "$INST2" != "$INST1" ]] && break
    sleep 1
  done
  sleep 2  # keep hammering a moment past the flip (into the drain window)
  touch "$HAMMER_STOP"; wait "$HPID" 2>/dev/null

  TOTAL="$(wc -l < "$HAMMER_OUT" | tr -d ' ')"
  # grep -c always prints a count; it also exits 1 when that count is 0, so do
  # NOT chain `|| echo 0` (that would append a second line and break the test).
  BAD="$(grep -vc '^200$' "$HAMMER_OUT" 2>/dev/null)"; BAD="${BAD:-0}"
  GEN2="$(appgen)"

  if [[ "$INST2" == sbx_* && "$INST2" != "$INST1" ]]; then
    pass "route flipped to a new instance ($INST1 → $INST2)"
  else
    fail "no flip: instance stayed $INST1 (INST2=${INST2:-none})"; tail -40 "$DAEMON_LOG"
  fi
  [[ "$GEN2" == "2" && "$GEN1" == "1" ]] && pass "generation bumped ($GEN1 → $GEN2)" \
    || fail "generation not bumped (was $GEN1, now $GEN2)"
  if [[ "$TOTAL" -gt 10 && "$BAD" -eq 0 ]]; then
    pass "zero-downtime: $TOTAL requests across the update, 0 non-200"
  else
    fail "dropped requests across the update: $BAD non-200 of $TOTAL"
    echo "   --- non-200 codes seen ---"; grep -v '^200$' "$HAMMER_OUT" | sort | uniq -c
    tail -40 "$DAEMON_LOG"
  fi
fi

# ---- 03 the superseded instance is drained then reaped ----------------------
echo "== 03 old instance reaped after the drain window (back to one instance)"
REAPED=0
for _ in $(seq 1 40); do
  # Only the current instance should remain for the app; INST1 is gone.
  if [[ "$(appinst)" == sbx_* ]] && ! api "$BASE_URL/sandboxes/$INST1" 2>/dev/null | grep -q "sbx_"; then
    REAPED=1; break
  fi
  sleep 1
done
[[ "$REAPED" -eq 1 ]] && pass "superseded instance $INST1 reaped after the drain window" \
  || fail "superseded instance $INST1 still present after the drain window"

# ---- 04 operate the app BY NAME: exec + logs resolve server-side ------------
echo "== 04 operate 'web' by name: app exec + app logs → current instance"
EXECOUT="$(cli app exec web -- /bin/sh -c 'echo ZDT-EXEC-OK' 2>/dev/null)"
[[ "$EXECOUT" == *ZDT-EXEC-OK* ]] && pass "app exec web resolved to the current instance and ran" \
  || fail "app exec web failed (got: ${EXECOUT:-empty})"
if cli app logs web 2>/dev/null | grep -q 'ZDT-EXEC-OK'; then
  pass "app logs web tailed the current instance's activity"
else
  fail "app logs web did not show the exec activity"
fi

cli app rm web >/dev/null 2>&1 || true

echo "==============================================================="
echo " zero-downtime smoke: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
