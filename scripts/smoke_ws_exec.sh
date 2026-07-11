#!/usr/bin/env bash
#
# WebSocket interactive-exec smoke.
#
# Proves the cross-language interactive transport (GET /sandboxes/{id}/exec +
# WebSocket upgrade) against a real KVM guest, using scripts/wsexec — a
# minimal client that speaks the exact contract a TS/Python SDK would (first
# message = JSON ExecRequest, then binary messages whose concatenated
# payloads are the wire frame stream).
#
#   01  daemon healthy (images + durable logs)
#   02  boot a sandbox from an OCI image (fresh embedded agent is injected)
#   03  WS stdin round-trip: a command typed on stdin produces output
#   04  WS shared state: `cd /tmp` persists into a later `pwd`
#   05  WS exit code propagates
#   06  plain GET (no upgrade handshake) answers 426
#   07  WS activity is captured in `crucible logs --source exec`
#   08  hijacked interactive + one-shot exec unaffected (no regression)
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built with
# an embedded agent (make build), the Go toolchain (builds scripts/wsexec;
# override with GO=/path/to/go or a prebuilt WSEXEC_BIN=…), curl, and network
# to pull the image (or a cached/local IMAGE).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_ws_exec.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7890}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-alpine:latest}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-wsexec-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible WebSocket interactive-exec smoke"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " listen     : $LISTEN"
echo " image      : $IMAGE"
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

# The guest side of interactive exec lives in the embedded agent; a stale
# build fails confusingly mid-stream, so check up front (same as smoke_shell).
if ! LC_ALL=C grep -qa "interactive exec completed" "$CRUCIBLE_BIN"; then
  echo "error: $CRUCIBLE_BIN has no interactive-exec agent embedded." >&2
  echo "       Rebuild with the current agent: make build" >&2
  exit 2
fi

# The WS route ships with the daemon — a binary from before it answers 405
# on the handshake, which reads like a protocol bug. Grep for the handler's
# error string so a stale build fails clearly up front.
if ! LC_ALL=C grep -qa "invalid exec request json" "$CRUCIBLE_BIN"; then
  echo "error: $CRUCIBLE_BIN predates the WebSocket exec endpoint." >&2
  echo "       Rebuild: make build" >&2
  exit 2
fi

# Build the WS client helper (or take a prebuilt one). Root's PATH often
# lacks go — probe the usual install locations before giving up.
WSEXEC_BIN="${WSEXEC_BIN:-}"
if [[ -z "$WSEXEC_BIN" ]]; then
  GO="${GO:-}"
  if [[ -z "$GO" ]]; then
    for cand in "$(command -v go 2>/dev/null)" /usr/local/go/bin/go /usr/lib/go/bin/go; do
      [[ -n "$cand" && -x "$cand" ]] && { GO="$cand"; break; }
    done
  fi
  [[ -n "$GO" ]] || { echo "error: go not found; set GO=/path/to/go or WSEXEC_BIN=…" >&2; exit 2; }
  "$GO" build -o "$SMOKE_ROOT/wsexec" ./scripts/wsexec || { echo "error: building scripts/wsexec failed" >&2; exit 2; }
  WSEXEC_BIN="$SMOKE_ROOT/wsexec"
fi

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }
wsx()  { "$WSEXEC_BIN" -addr "$LISTEN" "$@"; }

# ---- daemon -----------------------------------------------------------------

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --network-egress-iface "$EGRESS_IFACE" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..100}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}
final_cleanup() {
  for id in $(curl -sf "$BASE_URL/sandboxes" 2>/dev/null |
    python3 -c 'import json,sys;[print(s["id"]) for s in json.load(sys.stdin)["sandboxes"]]' 2>/dev/null); do
    curl -sf -X DELETE "$BASE_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null
  [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$SMOKE_ROOT"
}
trap final_cleanup EXIT

echo "== 01 starting daemon (images + durable logs)"
start_daemon
pass "daemon healthy"

# ---- 02 boot a sandbox from an image ----------------------------------------

echo "== 02 create a sandbox from $IMAGE (fresh embedded agent injected)"
SBX="$(cli sandbox create --image "$IMAGE" --memory 256)"
if [[ "$SBX" != sbx_* ]]; then
  fail "create --image $IMAGE failed: $SBX"; tail -30 "$DAEMON_LOG"; exit 3
fi
pass "booted $SBX from $IMAGE"

# ---- 03 WS stdin round-trip -------------------------------------------------

echo "== 03 WebSocket stdin round-trip"
OUT="$(printf 'echo hi-over-ws\nexit\n' | wsx -id "$SBX" -- /bin/sh 2>&1)"
if [[ "$OUT" == *"hi-over-ws"* ]]; then
  pass "command typed on stdin produced output over WS"
else
  fail "no WS stdin round-trip; got: $OUT"
fi

# ---- 04 WS shared session state ----------------------------------------------

echo "== 04 WS shared state (cd persists into a later pwd)"
OUT="$(printf 'cd /tmp\npwd\nexit\n' | wsx -id "$SBX" -- /bin/sh 2>&1)"
if [[ "$OUT" == *"/tmp"* ]]; then
  pass "cd state persisted across commands in one WS session"
else
  fail "WS shared state lost; got: $OUT"
fi

# ---- 05 WS exit-code propagation ---------------------------------------------

echo "== 05 exit code propagates over WS"
printf 'exit 7\n' | wsx -id "$SBX" -- /bin/sh >/dev/null 2>&1
rc=$?
if [[ "$rc" -eq 7 ]]; then
  pass "WS exit code 7 propagated"
else
  fail "expected exit 7 over WS, got $rc"
fi

# ---- 06 plain GET answers 426 -------------------------------------------------

echo "== 06 plain GET (no upgrade handshake) answers 426"
CODE="$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/sandboxes/$SBX/exec")"
if [[ "$CODE" == "426" ]]; then
  pass "non-WebSocket GET rejected with 426"
else
  fail "plain GET = $CODE, want 426"
fi

# ---- 07 WS activity captured in durable logs ----------------------------------

echo "== 07 WS activity captured in logs --source exec"
sleep 1
LOGS="$(cli logs "$SBX" --source exec 2>&1 || true)"
if [[ "$LOGS" == *"interactive"* || "$LOGS" == *"hi-over-ws"* ]]; then
  pass "WS exec activity teed into the durable log store"
else
  fail "WS activity missing from logs; got: $LOGS"
fi

# ---- 08 hijack + one-shot paths unaffected ------------------------------------

echo "== 08 hijacked interactive + one-shot exec unaffected"
OUT="$(printf 'echo hijack-still-ok\nexit\n' | cli sandbox exec -i "$SBX" -- /bin/sh 2>&1)"
if [[ "$OUT" == *"hijack-still-ok"* ]] &&
   OUT2="$(cli sandbox exec "$SBX" -- /bin/echo one-shot-ok 2>&1)" &&
   [[ "$OUT2" == *"one-shot-ok"* ]]; then
  pass "hijack and one-shot exec paths unchanged"
else
  fail "regression in existing exec paths; hijack: $OUT / one-shot: ${OUT2:-n/a}"
fi

# ---- summary ----------------------------------------------------------------

echo
echo "==============================================================="
echo " smoke_ws_exec: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
