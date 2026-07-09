#!/usr/bin/env bash
#
# End-to-end smoke test for the supervised-service API.
#
# Scenarios:
#
#   01  create-with-service: POST /sandboxes with a service block boots
#       a python http.server, sandbox arrives with state=running
#   02  the service actually serves: in-guest HTTP GET succeeds
#   03  logs: captured stdout/stderr readable, cursor resume works
#   04  restart policy: kill the process in-guest (restart=always)
#       → supervisor relaunches it with a new pid, restarts >= 1
#   05  graceful stop: service stop → stopped, last_exit_requested,
#       SIGTERM death recorded as 128+15
#   06  grace escalation: TERM-ignoring service + stop --grace 2
#       → SIGKILL recorded, stopped
#   07  on-failure budget: exit-1 service with on-failure:2 → failed,
#       restarts == 2
#   08  ring eviction is explicit: tiny log buffer + output flood
#       → dropped_records > 0, first_seq > 0
#   09  CLI verbs: crucible service set --start / status / logs / restart
#   10  snapshot + fork with a running service: the fork's service is
#       still running (supervisor state survives restore) and serves
#
# Requires (same as smoke_e2e.sh):
#   - Linux host with KVM + root (jailer needs CAP_SYS_ADMIN)
#   - Guest rootfs with the CURRENT crucible-agent (rebuild after any
#     agent change: make agent && rootfs rebuild) + python3
#   - crucible binaries built (make build && make agent)
#
# Usage:
#   sudo CRUCIBLE_BIN=./crucible \
#        FIRECRACKER_BIN=/path/to/firecracker \
#        JAILER_BIN=/path/to/jailer \
#        KERNEL=/path/to/vmlinux \
#        ROOTFS=./assets/rootfs-with-agent.ext4 \
#        scripts/smoke_service.sh

set -u
set -o pipefail

# ===== Configuration =========================================

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-./assets/rootfs-with-agent.ext4}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
WORK_BASE="${WORK_BASE:-/var/lib/crucible/run}"
LISTEN="${LISTEN:-127.0.0.1:7881}"
BASE_URL="http://${LISTEN}"

SMOKE_ROOT="/tmp/crucible-smoke-service-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$SMOKE_ROOT"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
SESSION_LOG="$SMOKE_ROOT/session.log"

exec > >(tee -a "$SESSION_LOG") 2>&1

echo "==============================================================="
echo " crucible supervised-service smoke"
echo "==============================================================="
echo " output dir       : $SMOKE_ROOT"
echo " crucible binary  : $CRUCIBLE_BIN"
echo " kernel / rootfs  : $KERNEL / $ROOTFS"
echo " listen           : $LISTEN"
echo "==============================================================="

# ===== Preflight =============================================

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (jailer requires CAP_SYS_ADMIN)" >&2
  exit 2
fi
for bin in "$CRUCIBLE_BIN" "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing or non-executable: $bin" >&2; exit 2; }
done
for f in "$KERNEL" "$ROOTFS"; do
  [[ -r "$f" ]] || { echo "error: missing or unreadable: $f" >&2; exit 2; }
done

# jq-like helper via python3 (no jq dependency).
jpath() {
  local file="$1"; shift
  python3 -c "
import json,sys
d = json.load(open('$file'))
for k in sys.argv[1:]:
    if k.isdigit():
        d = d[int(k)]
    else:
        d = d[k]
print(d)
" "$@"
}

# ===== Daemon lifecycle ======================================

DAEMON_PID=""

start_daemon() {
  echo "== starting daemon"
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" \
    --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" \
    --kernel "$KERNEL" \
    --rootfs "$ROOTFS" \
    --work-base "$WORK_BASE" \
    --log-format json --log-level info \
    >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  echo "   daemon pid: $DAEMON_PID"
  for _ in {1..100}; do
    if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
      echo "   healthy"
      return 0
    fi
    if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
      echo "error: daemon exited before becoming healthy" >&2
      tail -40 "$DAEMON_LOG" >&2
      exit 3
    fi
    sleep 0.2
  done
  echo "error: daemon never became healthy" >&2
  tail -40 "$DAEMON_LOG" >&2
  exit 3
}

stop_daemon() {
  if [[ -n "${DAEMON_PID:-}" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    echo "== stopping daemon (pid $DAEMON_PID)"
    kill -TERM "$DAEMON_PID" 2>/dev/null || true
    for _ in {1..50}; do
      kill -0 "$DAEMON_PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    DAEMON_PID=""
  fi
}

final_cleanup() {
  # Delete any sandboxes we leaked so the daemon can tear down cleanly.
  for id in $(curl -sf "$BASE_URL/sandboxes" 2>/dev/null |
    python3 -c 'import json,sys; [print(s["id"]) for s in json.load(sys.stdin)["sandboxes"]]' 2>/dev/null); do
    curl -sf -X DELETE "$BASE_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  stop_daemon
}
trap final_cleanup EXIT

# ===== Test harness ==========================================

PASS=0
FAIL=0

pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }

# svc <id> <verb> [curl args...] — service API helper writing the JSON
# response to $RESP.
RESP="$SMOKE_ROOT/resp.json"
svc_put()  { curl -sS -o "$RESP" -X PUT  "$BASE_URL/sandboxes/$1/service" -H 'Content-Type: application/json' -d "$2"; }
svc_post() { curl -sS -o "$RESP" -X POST "$BASE_URL/sandboxes/$1/service/$2" ${3:+-H 'Content-Type: application/json' -d "$3"}; }
svc_get()  { curl -sS -o "$RESP" "$BASE_URL/sandboxes/$1/service${2:-}"; }

# exec_in <id> <cmd...> — one-shot exec via the CLI; stdout to stdout.
exec_in() {
  local id="$1"; shift
  "$CRUCIBLE_BIN" --addr "$LISTEN" sandbox exec "$id" --timeout 30 -- "$@"
}

# wait_guest_http <id> <port> [tries] — poll an in-guest HTTP GET until
# it returns 200. state=running means the process is alive, not that
# the listener is bound yet (readiness/health checks are future work) —
# python needs a beat to bind, and a restored fork needs a beat to settle.
wait_guest_http() {
  local id="$1" port="$2" tries="${3:-20}"
  local body
  for _ in $(seq 1 "$tries"); do
    body="$(exec_in "$id" python3 -c "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:$port/', timeout=5).status)" 2>/dev/null || true)"
    if [[ "$body" == *200* ]]; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

# wait_state <id> <state> [tries] — poll service state.
wait_state() {
  local id="$1" want="$2" tries="${3:-100}"
  for _ in $(seq 1 "$tries"); do
    svc_get "$id"
    if [[ "$(jpath "$RESP" state)" == "$want" ]]; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

start_daemon

# ===== 01: create-with-service ================================

echo "== 01 create-with-service boots running"
CREATE="$SMOKE_ROOT/create.json"
curl -sS -o "$CREATE" -X POST "$BASE_URL/sandboxes" -H 'Content-Type: application/json' -d '{
  "memory_mib": 512,
  "timeout_s": 0,
  "service": {
    "cmd": ["python3", "-m", "http.server", "8000"],
    "env": {"PYTHONUNBUFFERED": "1"},
    "restart": {"policy": "always"}
  }
}'
SBX="$(jpath "$CREATE" id 2>/dev/null || true)"
if [[ -n "$SBX" && "$SBX" == sbx_* ]]; then
  pass "created $SBX with service"
else
  fail "create-with-service: $(cat "$CREATE")"
  echo "cannot continue without a sandbox"; exit 1
fi

if wait_state "$SBX" running 25; then
  pass "service state running (pid $(jpath "$RESP" pid))"
else
  fail "service never reached running: $(cat "$RESP")"
fi

# ===== 02: the service actually serves ========================

echo "== 02 in-guest HTTP round-trip"
if wait_guest_http "$SBX" 8000; then
  pass "http.server answered 200 in-guest"
else
  fail "in-guest GET never returned 200 (listener never bound?)"
fi

# ===== 03: logs + cursor ======================================

echo "== 03 logs capture + cursor resume"
# The GET in 02 produces a request log on stderr; PYTHONUNBUFFERED=1
# in the spec makes the stdout banner visible too. Poll briefly — the
# pipe hop from guest process to ring is fast but not synchronous.
LOGS_OK=""
for _ in {1..20}; do
  svc_get "$SBX" "/logs?from_seq=0"
  if python3 -c "
import json,base64,sys
d=json.load(open('$RESP'))
data=b''.join(base64.b64decode(r['data']) for r in (d.get('records') or []))
sys.exit(0 if b'GET /' in data or b'Serving HTTP' in data else 1)
"; then
    LOGS_OK=1
    break
  fi
  sleep 0.5
done
NEXT="$(jpath "$RESP" next_seq)"
if [[ -n "$LOGS_OK" ]]; then
  pass "logs contain http.server output (next_seq=$NEXT)"
else
  fail "logs missing expected output: $(cat "$RESP")"
fi
svc_get "$SBX" "/logs?from_seq=$NEXT"
if [[ "$(jpath "$RESP" next_seq)" -ge "$NEXT" ]]; then
  pass "cursor resume works"
else
  fail "cursor resume: $(cat "$RESP")"
fi

# ===== 04: restart policy relaunches ==========================

echo "== 04 kill in-guest, restart=always relaunches"
svc_get "$SBX"
OLD_PID="$(jpath "$RESP" pid)"
exec_in "$SBX" sh -c "kill -9 $OLD_PID" >/dev/null 2>&1 || true
NEW_PID=""
for _ in {1..50}; do
  svc_get "$SBX"
  if [[ "$(jpath "$RESP" state)" == "running" ]]; then
    NEW_PID="$(jpath "$RESP" pid)"
    [[ "$NEW_PID" != "$OLD_PID" ]] && break
  fi
  sleep 0.2
done
RESTARTS="$(jpath "$RESP" restarts 2>/dev/null || echo 0)"
if [[ -n "$NEW_PID" && "$NEW_PID" != "$OLD_PID" && "$RESTARTS" -ge 1 ]]; then
  pass "relaunched: pid $OLD_PID → $NEW_PID, restarts=$RESTARTS"
else
  fail "no policy relaunch observed: $(cat "$RESP")"
fi

# ===== 05: graceful stop ======================================

echo "== 05 stop → requested SIGTERM death"
svc_post "$SBX" stop
if wait_state "$SBX" stopped 50; then
  REQ="$(jpath "$RESP" last_exit_requested 2>/dev/null || echo False)"
  SIG="$(jpath "$RESP" last_exit signal 2>/dev/null || echo '')"
  CODE="$(jpath "$RESP" last_exit exit_code 2>/dev/null || echo '')"
  if [[ "$REQ" == "True" && "$SIG" == "SIGTERM" && "$CODE" == "143" ]]; then
    pass "stopped: requested, SIGTERM, exit 143"
  else
    fail "stop reported requested=$REQ signal=$SIG code=$CODE"
  fi
else
  fail "service never stopped: $(cat "$RESP")"
fi

# ===== 06: grace escalation ===================================

echo "== 06 TERM-ignorer escalates to SIGKILL"
svc_put "$SBX" '{"cmd":["sh","-c","trap \"\" TERM; while :; do sleep 0.2; done"],"stop_grace_s":2}'
svc_post "$SBX" start
wait_state "$SBX" running 25 || fail "TERM-ignorer never started"
svc_post "$SBX" stop
if wait_state "$SBX" stopped 60; then
  SIG="$(jpath "$RESP" last_exit signal 2>/dev/null || echo '')"
  if [[ "$SIG" == "SIGKILL" ]]; then
    pass "escalated to SIGKILL after grace"
  else
    fail "expected SIGKILL death, got: $(cat "$RESP")"
  fi
else
  fail "TERM-ignorer never stopped: $(cat "$RESP")"
fi

# ===== 07: on-failure budget exhausts =========================

echo "== 07 on-failure:2 exhausts to failed"
svc_put "$SBX" '{"cmd":["sh","-c","exit 1"],"restart":{"policy":"on-failure","max_retries":2}}'
svc_post "$SBX" start
if wait_state "$SBX" failed 60; then
  RESTARTS="$(jpath "$RESP" restarts)"
  if [[ "$RESTARTS" == "2" ]]; then
    pass "failed after exactly 2 policy restarts"
  else
    fail "failed with restarts=$RESTARTS, want 2"
  fi
else
  fail "never reached failed: $(cat "$RESP")"
fi

# ===== 08: ring eviction is explicit ==========================

echo "== 08 log flood: eviction reported, no silent hole"
svc_put "$SBX" '{"cmd":["sh","-c","i=0; while [ $i -lt 5000 ]; do echo line-$i-padding-padding-padding-padding; i=$((i+1)); done; sleep 60"],"log_buffer_bytes":65536}'
svc_post "$SBX" start
wait_state "$SBX" running 25 || true
sleep 3
svc_get "$SBX" "/logs?from_seq=0"
DROPPED="$(jpath "$RESP" dropped_records 2>/dev/null || echo 0)"
FIRST="$(jpath "$RESP" first_seq 2>/dev/null || echo 0)"
if [[ "$DROPPED" -gt 0 && "$FIRST" -gt 0 ]]; then
  pass "eviction explicit: dropped=$DROPPED first_seq=$FIRST"
else
  fail "flood produced no visible eviction: dropped=$DROPPED first_seq=$FIRST"
fi
svc_post "$SBX" stop
wait_state "$SBX" stopped 50 || true

# ===== 09: CLI verbs ==========================================

echo "== 09 CLI: service set --start / status / logs / restart"
cli() { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }
if cli service set "$SBX" --start --restart always --env PYTHONUNBUFFERED=1 -- python3 -m http.server 8001 >/dev/null; then
  pass "service set --start"
else
  fail "service set --start"
fi
if cli service status "$SBX" | grep -q "state: running"; then
  pass "service status shows running"
else
  fail "service status"
fi
LOGS_OK=""
for _ in {1..20}; do
  if cli service logs "$SBX" 2>&1 | grep -q "8001"; then
    LOGS_OK=1
    break
  fi
  sleep 0.5
done
if [[ -n "$LOGS_OK" ]]; then
  pass "service logs show listener output"
else
  fail "service logs"
fi
if cli service restart "$SBX" >/dev/null && cli service status "$SBX" | grep -q "state: running"; then
  pass "service restart"
else
  fail "service restart"
fi

# ===== 10: snapshot + fork with a running service =============

echo "== 10 fork carries the running service"
SNAP_JSON="$SMOKE_ROOT/snap.json"
curl -sS -o "$SNAP_JSON" -X POST "$BASE_URL/sandboxes/$SBX/snapshot"
SNAP="$(jpath "$SNAP_JSON" id 2>/dev/null || true)"
if [[ "$SNAP" == snap_* ]]; then
  pass "snapshot $SNAP taken with service running"
else
  fail "snapshot: $(cat "$SNAP_JSON")"
fi
FORK_JSON="$SMOKE_ROOT/fork.json"
curl -sS -o "$FORK_JSON" -X POST "$BASE_URL/snapshots/$SNAP/fork?count=1"
FORK="$(jpath "$FORK_JSON" sandboxes 0 id 2>/dev/null || true)"
if [[ "$FORK" == sbx_* ]]; then
  if wait_state "$FORK" running 25; then
    pass "fork $FORK service still running (supervisor survived restore)"
  else
    fail "fork service state: $(cat "$RESP")"
  fi
  if wait_guest_http "$FORK" 8001; then
    pass "fork's service serves in-guest"
  else
    fail "fork in-guest GET never returned 200"
  fi
else
  fail "fork: $(cat "$FORK_JSON")"
fi

# ===== Summary ================================================

echo "==============================================================="
echo " service smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
