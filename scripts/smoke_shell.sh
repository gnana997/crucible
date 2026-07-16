#!/usr/bin/env bash
#
# Interactive-exec (shell) smoke.
#
# Proves the "explore untrusted code" UX: a real, long-lived shell into a
# running sandbox over a hijacked full-duplex frame stream (stdin ↔ output),
# with persistent session state and no PTY.
#
#   01  daemon healthy (images + durable logs)
#   02  boot a sandbox from an OCI image (fresh embedded agent is injected)
#   03  interactive stdin round-trip: a command typed on stdin produces output
#   04  shared state: `cd /tmp` in one command persists into a later `pwd`
#   05  exit code propagates from the interactive session
#   06  `crucible shell <id>` alias opens a working shell
#   07  interactive activity is captured in `crucible logs --source exec`
#   08  the one-shot `sandbox exec` path is unaffected (no regression)
#
# We boot from an **OCI image** (not a profile .ext4) on purpose: the daemon
# injects the agent embedded in the crucible binary (`make build`), so this
# always runs the current agent — and it exercises the PID-1 (init-mode)
# interactive handler that images actually use. A profile .ext4 would need a
# rootfs rebuild to pick up agent changes (see agent-rootfs-baking).
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built with
# an embedded agent (make build), curl, and network to pull the image (or a
# cached/local IMAGE).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_shell.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7887}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-alpine:latest}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-shell-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible interactive-shell smoke"
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

# Interactive exec is a guest-agent feature: the embedded agent the daemon
# injects into images must be new enough to have the interactive handler.
# Grep the crucible binary (it carries the embedded agent) up front so a
# stale build fails clearly instead of "stream ended without an exit frame".
if ! LC_ALL=C grep -qa "interactive exec completed" "$CRUCIBLE_BIN"; then
  echo "error: $CRUCIBLE_BIN has no interactive-exec agent embedded." >&2
  echo "       Rebuild with the current agent: make build" >&2
  exit 2
fi

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

# ---- daemon -----------------------------------------------------------------

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --app-db "$WORK_BASE-apps.db" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
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
  # SMOKE_ROOT lives under /tmp; drop the transcripts unless KEEP=1.
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

# ---- 03 stdin round-trip ----------------------------------------------------

echo "== 03 interactive stdin round-trip"
OUT="$(printf 'echo hi-from-stdin\nexit\n' | cli sandbox exec -i "$SBX" -- /bin/sh 2>&1)"
if [[ "$OUT" == *"hi-from-stdin"* ]]; then
  pass "command typed on stdin produced output"
else
  fail "no stdin round-trip; got: $OUT"
fi

# ---- 04 shared session state ------------------------------------------------

echo "== 04 shared state (cd persists into a later pwd)"
OUT="$(printf 'cd /tmp\npwd\nexit\n' | cli sandbox exec -i "$SBX" -- /bin/sh 2>&1)"
if [[ "$OUT" == *"/tmp"* ]]; then
  pass "cd state persisted across commands in one session"
else
  fail "shared state lost; got: $OUT"
fi

# ---- 05 exit-code propagation -----------------------------------------------

echo "== 05 exit code propagates from the interactive session"
printf 'exit 7\n' | cli sandbox exec -i "$SBX" -- /bin/sh >/dev/null 2>&1
rc=$?
if [[ "$rc" -eq 7 ]]; then
  pass "interactive exit code 7 propagated"
else
  fail "expected exit 7, got $rc"
fi

# ---- 06 `crucible shell` alias ----------------------------------------------

echo "== 06 crucible shell <id> alias"
OUT="$(printf 'echo shell-alias-ok\nexit\n' | cli shell "$SBX" 2>&1)"
if [[ "$OUT" == *"shell-alias-ok"* ]]; then
  pass "crucible shell alias opened a working shell"
else
  fail "shell alias failed; got: $OUT"
fi

# ---- 07 interactive activity captured in durable logs -----------------------

echo "== 07 interactive activity captured in logs --source exec"
sleep 1
LOGS="$(cli logs "$SBX" --source exec 2>&1 || true)"
if [[ "$LOGS" == *"interactive"* || "$LOGS" == *"hi-from-stdin"* || "$LOGS" == *"shell-alias-ok"* ]]; then
  pass "exec activity teed into the durable log store"
else
  fail "interactive activity missing from logs; got: $LOGS"
fi

# ---- 08 one-shot exec still works (no regression) ---------------------------

echo "== 08 one-shot exec path unaffected"
if OUT="$(cli sandbox exec "$SBX" -- /bin/echo one-shot-ok 2>&1)" && [[ "$OUT" == *"one-shot-ok"* ]]; then
  pass "non-interactive exec unchanged"
else
  fail "one-shot exec regressed; got: $OUT"
fi

# ---- summary ----------------------------------------------------------------

echo "==============================================================="
echo " shell smoke: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
