#!/usr/bin/env bash
#
# Durable, tailable per-sandbox logs smoke.
#
# Boots a service that prints a known line then ticks, and proves:
#   02  create from a local image (D8 auto-import)
#   03  crucible logs <id>            → the service's startup line (durable app log)
#   04  crucible logs <id> -f         → new ticks stream in (follow)
#   05  crucible exec + logs --source exec → the exec output + its activity events
#   06  delete the sandbox, logs still readable (durability past teardown)
#
# No networking required — logs ride vsock, not the NIC.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built
# (make build), docker, curl.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_logs.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7885}"
BASE_URL="http://${LISTEN}"
HERE="$(cd "$(dirname "$0")" && pwd)"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-logs-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible durable logs smoke"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v docker >/dev/null || { echo "error: docker needed" >&2; exit 2; }
command -v curl   >/dev/null || { echo "error: curl needed" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

# logs_contain <id> <needle> [source] — poll `crucible logs` until it shows needle.
logs_contain() {
  local id="$1" needle="$2" src="${3:-all}" body
  for _ in {1..40}; do
    body="$(cli logs "$id" --source "$src" 2>/dev/null || true)"
    [[ "$body" == *"$needle"* ]] && return 0
    sleep 0.5
  done
  return 1
}

# ---- daemon -----------------------------------------------------------------

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..50}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -20 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3
}
final_cleanup() {
  for id in $(curl -sf "$BASE_URL/sandboxes" 2>/dev/null |
    python3 -c 'import json,sys;[print(s["id"]) for s in json.load(sys.stdin)["sandboxes"]]' 2>/dev/null); do
    curl -sf -X DELETE "$BASE_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null
  [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null
  rm -rf "$SMOKE_ROOT"
}
trap final_cleanup EXIT

echo "== 01 starting daemon (images + durable logs, no networking)"
start_daemon
pass "daemon healthy with --log-dir"

# ---- 02 build + create ------------------------------------------------------

echo "== 02 build the log-test image + create a sandbox (D8 auto-import)"
if ! docker build -q -t crucible-logtest "$HERE/logtest" >/dev/null 2>"$SMOKE_ROOT/build.err"; then
  fail "docker build: $(cat "$SMOKE_ROOT/build.err")"; exit 1
fi
if SBX="$(cli sandbox create --image crucible-logtest --memory 256)" && [[ "$SBX" == sbx_* ]]; then
  pass "created $SBX from local image"
else
  fail "create failed"; tail -30 "$DAEMON_LOG"; exit 1
fi

# ---- 03 durable app log -----------------------------------------------------

echo "== 03 crucible logs shows the service's startup line"
if logs_contain "$SBX" "CRUCIBLE-LOG-HELLO" service; then
  pass "service startup line captured (durable app log)"
else
  fail "startup line not found in logs"; tail -20 "$DAEMON_LOG"
fi

# ---- 04 follow --------------------------------------------------------------

echo "== 04 crucible logs -f streams new output"
FOLLOW_OUT="$SMOKE_ROOT/follow.out"
cli logs "$SBX" -f --source service >"$FOLLOW_OUT" 2>/dev/null &
FOLLOW_PID=$!
sleep 7
kill -TERM "$FOLLOW_PID" 2>/dev/null; wait "$FOLLOW_PID" 2>/dev/null
TICKS="$(grep -c 'CRUCIBLE-TICK' "$FOLLOW_OUT" 2>/dev/null || echo 0)"
if [[ "$TICKS" -ge 2 ]]; then
  pass "follow streamed $TICKS ticks"
else
  fail "follow captured $TICKS ticks (<2)"; echo "--- follow.out ---"; cat "$FOLLOW_OUT"
fi

# ---- 05 exec activity -------------------------------------------------------

echo "== 05 exec + logs --source exec shows the invocation and output"
cli sandbox exec "$SBX" -- echo CRUCIBLE-EXEC-OK >/dev/null 2>&1 || true
if logs_contain "$SBX" "CRUCIBLE-EXEC-OK" exec; then
  pass "exec output captured under --source exec"
else
  fail "exec output not found in exec logs"
fi
if cli logs "$SBX" --source exec 2>/dev/null | grep -q 'exec: echo CRUCIBLE-EXEC-OK'; then
  pass "exec invocation event recorded"
else
  fail "exec invocation event missing"
fi

# ---- 06 durability past delete ----------------------------------------------

echo "== 06 logs survive sandbox deletion"
cli sandbox rm "$SBX" >/dev/null 2>&1
sleep 1
if cli logs "$SBX" --source service 2>/dev/null | grep -q 'CRUCIBLE-LOG-HELLO'; then
  pass "logs readable after the sandbox is gone (durable)"
else
  fail "logs lost after deletion"
fi

# ---- summary ----------------------------------------------------------------

echo "==============================================================="
echo " logs smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT (removed on exit)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
