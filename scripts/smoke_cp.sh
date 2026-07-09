#!/usr/bin/env bash
#
# crucible cp (push) smoke.
#
# Proves the "drop my files in and run them" loop: copy a local file and a
# local directory into a running sandbox and confirm they land with the right
# content — no image build, no Dockerfile.
#
#   01  daemon healthy (images)
#   02  boot a sandbox from an OCI image (fresh embedded agent is injected)
#   03  cp a single file  → exec cat confirms its content in the guest
#   03b read the file back → GET /files returns its content (+ max_bytes caps it)
#   04  cp a directory    → exec confirms a nested file under <dest>/<basename>
#   05  cp overwrites an existing file in the guest
#   06  the one-shot exec path is unaffected (sanity)
#
# We boot from an OCI image (not a profile .ext4) on purpose so the daemon
# injects the agent embedded in the crucible binary (`make build`) — this always
# runs the current agent, including the PUT /files handler.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built with an
# embedded agent (make build), curl, and network to pull the image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_cp.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7886}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-alpine:latest}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-cp-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"
SRC_DIR="$SMOKE_ROOT/src"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR" "$SRC_DIR"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible cp (push) smoke"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " listen     : $LISTEN"
echo " image      : $IMAGE"
echo "==============================================================="

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }

# The PUT /files handler ships in the embedded agent; grep the binary up front so
# a stale build fails clearly instead of a confusing runtime error.
if ! LC_ALL=C grep -qa "destination dir in the guest" "$CRUCIBLE_BIN"; then
  echo "error: $CRUCIBLE_BIN has no cp/files agent embedded. Rebuild: make build" >&2
  exit 2
fi

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

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

echo "== 01 starting daemon"
start_daemon
pass "daemon healthy"

echo "== 02 create a sandbox from $IMAGE"
SBX="$(cli sandbox create --image "$IMAGE" --memory 256)"
if [[ "$SBX" != sbx_* ]]; then
  fail "create --image $IMAGE failed: $SBX"; tail -30 "$DAEMON_LOG"; exit 3
fi
pass "booted $SBX from $IMAGE"

# ---- 03 cp a single file ----------------------------------------------------
echo "== 03 cp a single file into /work"
printf 'CP-SINGLE-FILE-OK\n' > "$SRC_DIR/hello.txt"
if cli cp "$SRC_DIR/hello.txt" "$SBX:/work" 2>&1 | grep -q 'copied'; then
  OUT="$(cli sandbox exec "$SBX" -- cat /work/hello.txt 2>&1)"
  if [[ "$OUT" == *"CP-SINGLE-FILE-OK"* ]]; then
    pass "single file landed at /work/hello.txt with correct content"
  else
    fail "file content wrong: $OUT"
  fi
else
  fail "cp single file did not report success"
fi

# ---- 03b read a file back out (GET /files — the read_file/read plumbing) ----
echo "== 03b read the file's content back out (daemon GET /files)"
BODY="$(curl -s --max-time 5 "$BASE_URL/sandboxes/$SBX/files?path=/work/hello.txt" 2>&1)"
if [[ "$BODY" == *"CP-SINGLE-FILE-OK"* ]]; then
  pass "read the file's content back (guest → host, content only)"
else
  fail "read-back content wrong: $BODY"
fi
CAPPED="$(curl -s --max-time 5 "$BASE_URL/sandboxes/$SBX/files?path=/work/hello.txt&max_bytes=6" 2>&1)"
if [[ "$CAPPED" == "CP-SIN" ]]; then
  pass "max_bytes caps the read (got '$CAPPED')"
else
  fail "max_bytes cap wrong: '$CAPPED'"
fi

# ---- 04 cp a directory (recursive) ------------------------------------------
echo "== 04 cp a directory into /work"
mkdir -p "$SRC_DIR/proj/sub"
printf 'print("nested")\n' > "$SRC_DIR/proj/sub/app.py"
if cli cp "$SRC_DIR/proj" "$SBX:/work" 2>&1 | grep -q 'copied'; then
  OUT="$(cli sandbox exec "$SBX" -- cat /work/proj/sub/app.py 2>&1)"
  if [[ "$OUT" == *"nested"* ]]; then
    pass "directory copied recursively (/work/proj/sub/app.py present)"
  else
    fail "nested file missing/wrong: $OUT"
  fi
else
  fail "cp directory did not report success"
fi

# ---- 05 overwrite -----------------------------------------------------------
echo "== 05 cp overwrites an existing file"
printf 'CP-OVERWRITTEN\n' > "$SRC_DIR/hello.txt"
cli cp "$SRC_DIR/hello.txt" "$SBX:/work" >/dev/null 2>&1
OUT="$(cli sandbox exec "$SBX" -- cat /work/hello.txt 2>&1)"
if [[ "$OUT" == *"CP-OVERWRITTEN"* ]]; then
  pass "overwrite replaced the file content"
else
  fail "overwrite failed: $OUT"
fi

# ---- 06 one-shot exec sanity ------------------------------------------------
echo "== 06 one-shot exec still works"
if OUT="$(cli sandbox exec "$SBX" -- /bin/echo exec-ok 2>&1)" && [[ "$OUT" == *"exec-ok"* ]]; then
  pass "one-shot exec unaffected"
else
  fail "exec regressed: $OUT"
fi

echo "==============================================================="
echo " cp smoke: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
