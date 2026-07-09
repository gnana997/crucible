#!/usr/bin/env bash
#
# build + run smoke.
#
# Proves the two-line docker-parity story:
#   crucible build -t <tag> <context>   → image in the store (prints digest)
#   crucible run <digest> -p H:G        → boots it, publishes a port, long-lived
#
#   01  daemon up (images + networking)
#   02  crucible build the testapp context → prints a crucible digest
#   03  crucible run <digest> -p 8080:80 → prints a sandbox id (long-lived)
#   04  curl localhost:8080 from the host → CRUCIBLE-TESTAPP-OK (ingress works)
#   05  crucible stop <id> → the service stops gracefully (sandbox remains)
#   06  crucible rm <id>   → the sandbox is gone
#   07  crucible run <digest> --rm (background) → removes itself on detach
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built
# (make build), docker (for `crucible build`), curl, a default route.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_build_run.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7889}"
BASE_URL="http://${LISTEN}"
HERE="$(cd "$(dirname "$0")" && pwd)"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-buildrun-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible build + run smoke"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " listen     : $LISTEN"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v docker >/dev/null || { echo "error: docker needed for crucible build" >&2; exit 2; }
command -v curl   >/dev/null || { echo "error: curl needed" >&2; exit 2; }

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

hit_host() {
  local url="$1" body
  for _ in {1..30}; do
    body="$(curl -s --max-time 3 "$url" 2>/dev/null || true)"
    [[ "$body" == *CRUCIBLE-TESTAPP-OK* ]] && return 0
    sleep 0.5
  done
  return 1
}

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" \
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

echo "== 01 starting daemon"
start_daemon
pass "daemon healthy"

echo "== 02 crucible build the testapp context"
DIGEST="$(cli build -t crucible-buildrun "$HERE/testapp" 2>"$SMOKE_ROOT/build.err")"
if [[ "$DIGEST" == sha256:* ]]; then
  pass "build produced a crucible digest ($DIGEST)"
else
  fail "build did not print a digest: $DIGEST / $(cat "$SMOKE_ROOT/build.err")"; exit 1
fi

echo "== 03 crucible run <digest> -p 8080:80 (long-lived)"
SBX="$(cli run "$DIGEST" --memory 256 -p 8080:80)"
if [[ "$SBX" == sbx_* ]]; then
  pass "run booted $SBX from the built image"
else
  fail "run did not print a sandbox id: $SBX"; tail -30 "$DAEMON_LOG"; exit 1
fi

echo "== 04 curl the published port from the host"
if hit_host "http://localhost:8080/"; then
  pass "curl localhost:8080 → CRUCIBLE-TESTAPP-OK (ingress reached the microVM)"
else
  fail "published port unreachable"; tail -20 "$DAEMON_LOG"
fi

echo "== 05 crucible stop <id> (graceful; sandbox remains)"
if cli stop "$SBX" >/dev/null 2>"$SMOKE_ROOT/stop.err"; then
  if cli sandbox ls | grep -q "$SBX"; then
    pass "stop halted the service but kept the sandbox"
  else
    fail "sandbox vanished after stop (should remain)"
  fi
else
  fail "stop failed: $(cat "$SMOKE_ROOT/stop.err")"
fi

echo "== 06 crucible rm <id> (hard remove)"
cli rm "$SBX" >/dev/null 2>&1
sleep 1
if ! cli sandbox ls | grep -q "$SBX"; then
  pass "rm removed the sandbox"
else
  fail "sandbox still present after rm"
fi

echo "== 07 crucible run --rm removes itself on detach (Ctrl-C)"
# Invoke the binary DIRECTLY (not the cli function): a backgrounded function
# runs in a subshell, and with an EXIT trap set bash won't exec-optimize it,
# so $! would be the subshell, not crucible — SIGINT must reach crucible.
"$CRUCIBLE_BIN" --addr "$LISTEN" run "$DIGEST" --memory 256 --rm >/dev/null 2>&1 &
RUN_PID=$!
# Wait for the --rm sandbox to appear.
RM_SBX=""
for _ in {1..40}; do
  RM_SBX="$(cli sandbox ls 2>/dev/null | awk 'NR>1{print $1}' | head -1)"
  [[ -n "$RM_SBX" ]] && break
  sleep 0.5
done
# Detach: SIGINT crucible → context cancels → deferred DeleteSandbox runs.
kill -INT "$RUN_PID" 2>/dev/null
wait "$RUN_PID" 2>/dev/null
gone=0
for _ in {1..30}; do
  cli sandbox ls 2>/dev/null | grep -q "$RM_SBX" || { gone=1; break; }
  sleep 0.2
done
if [[ -n "$RM_SBX" && "$gone" == "1" ]]; then
  pass "--rm removed its sandbox on detach"
else
  fail "--rm left sandbox ${RM_SBX:-<none>} behind"
fi

echo "==============================================================="
echo " build+run smoke: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
