#!/usr/bin/env bash
#
# End-to-end smoke for booting converted OCI images (D2-m1).
#
# Boots a real Firecracker microVM from a D1-converted image with the
# crucible-agent as PID 1 (init mode), and validates the boot end to
# end: healthz over vsock, exec into the guest, PID 1 is the agent, and
# a supervised service runs. Networking for image sandboxes lands in
# D2-m2; this milestone is boot + exec + service, no network.
#
# Scenarios:
#   01  pull alpine and convert it (via the daemon image store)
#   02  create a sandbox from the image → boots, agent healthy over vsock
#   03  exec in the guest returns output (init-mode exec path)
#   04  PID 1 inside the guest is crucible-agent (init mode confirmed)
#   05  the guest has a populated /proc, /dev (pseudo-fs mounts worked)
#   06  create-with-service from the image runs a supervised entrypoint
#   07  cleanup leaves no sandboxes
#
# Requires: root + KVM, firecracker + jailer + a vmlinux, and crucible
# built with an embedded agent (make build). No registry mirror needed
# beyond docker.io for the alpine pull.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_oci.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7883}"
BASE_URL="http://${LISTEN}"

SMOKE_ROOT="/tmp/crucible-smoke-oci-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible OCI-boot smoke (init mode)"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " crucible   : $CRUCIBLE_BIN"
echo " kernel     : $KERNEL"
echo " image dir  : $IMAGE_DIR"
echo " listen     : $LISTEN"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (KVM + jailer)" >&2
  exit 2
fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (run make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }

jpath() {
  local file="$1"; shift
  python3 -c "
import json,sys
d = json.load(open('$file'))
for k in sys.argv[1:]:
    d = d[int(k)] if k.isdigit() else d[k]
print(d)
" "$@"
}

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }

cli() { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

# ---- daemon -----------------------------------------------------------------

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" \
    --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" \
    --kernel "$KERNEL" \
    --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" \
    --image-dir "$IMAGE_DIR" \
    --log-format json --log-level info \
    >>"$DAEMON_LOG" 2>&1 &
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
}
trap final_cleanup EXIT

# --rootfs is a required daemon flag but unused here (every sandbox names
# an image); we point it at the kernel file just to satisfy the check.
echo "== starting daemon"
start_daemon
pass "daemon healthy with image support"

# ---- 01 pull + convert ------------------------------------------------------

echo "== 01 pull + convert alpine"
if cli image pull alpine:latest -o json > "$SMOKE_ROOT/alpine.json" 2>"$SMOKE_ROOT/alpine.err"; then
  DIGEST="$(jpath "$SMOKE_ROOT/alpine.json" digest)"
  pass "converted alpine ($DIGEST)"
else
  fail "pull alpine: $(cat "$SMOKE_ROOT/alpine.err")"
  echo "cannot continue"; exit 1
fi

# ---- 02 boot from the image -------------------------------------------------

echo "== 02 create a sandbox from the image (boots agent as PID 1)"
if SBX="$(cli sandbox create --image "$DIGEST" --memory 256)"; then
  if [[ "$SBX" == sbx_* ]]; then
    pass "booted $SBX (create returned, agent answered /healthz over vsock)"
  else
    fail "unexpected create output: $SBX"; exit 1
  fi
else
  fail "create from image failed; daemon log:"; tail -30 "$DAEMON_LOG"; exit 1
fi

# ---- 03 exec ----------------------------------------------------------------

echo "== 03 exec in the guest"
OSREL="$(cli sandbox exec "$SBX" --timeout 20 -- /bin/cat /etc/os-release 2>/dev/null || true)"
if echo "$OSREL" | grep -qi alpine; then
  pass "exec ran; guest is alpine"
else
  fail "exec output unexpected: $(echo "$OSREL" | head -1)"
fi

# ---- 04 PID 1 is the agent --------------------------------------------------

echo "== 04 PID 1 is crucible-agent (init mode)"
PID1="$(cli sandbox exec "$SBX" --timeout 20 -- /bin/cat /proc/1/comm 2>/dev/null | tr -d '\r\n' || true)"
if [[ "$PID1" == crucible-agent* ]]; then
  pass "/proc/1/comm = $PID1"
else
  fail "PID 1 comm = '$PID1', want crucible-agent"
fi

# ---- 05 pseudo-filesystems mounted ------------------------------------------

echo "== 05 pseudo-filesystems mounted"
DEVNULL="$(cli sandbox exec "$SBX" --timeout 20 -- /bin/sh -c 'test -c /dev/null && echo ok' 2>/dev/null | tr -d '\r\n' || true)"
PROCOK="$(cli sandbox exec "$SBX" --timeout 20 -- /bin/sh -c 'test -d /proc/1 && echo ok' 2>/dev/null | tr -d '\r\n' || true)"
if [[ "$DEVNULL" == ok && "$PROCOK" == ok ]]; then
  pass "/dev and /proc populated (init mounts worked)"
else
  fail "mounts incomplete: /dev/null=$DEVNULL /proc=$PROCOK"
fi

# ---- 06 create-with-service from the image ----------------------------------

echo "== 06 create-with-service from the image"
SVC_JSON="$SMOKE_ROOT/svc-create.json"
curl -sS -o "$SVC_JSON" -X POST "$BASE_URL/sandboxes" -H 'Content-Type: application/json' -d "{
  \"memory_mib\": 256,
  \"image\": {\"oci\": \"$DIGEST\"},
  \"service\": {\"cmd\": [\"/bin/sh\", \"-c\", \"while :; do sleep 0.5; done\"], \"restart\": {\"policy\": \"always\"}}
}"
SVC_SBX="$(jpath "$SVC_JSON" id 2>/dev/null || true)"
if [[ "$SVC_SBX" == sbx_* ]]; then
  sleep 1
  STATE="$(curl -sf "$BASE_URL/sandboxes/$SVC_SBX/service" | jpath /dev/stdin state 2>/dev/null || true)"
  if [[ "$STATE" == "running" ]]; then
    pass "service supervised as PID-1 child (state running)"
  else
    fail "service state = '$STATE', want running"
  fi
else
  fail "create-with-service from image: $(cat "$SVC_JSON")"
fi

# ---- 07 cleanup -------------------------------------------------------------

echo "== 07 cleanup"
for id in "$SBX" "${SVC_SBX:-}"; do
  [[ -n "$id" ]] && cli sandbox rm "$id" >/dev/null 2>&1
done
REMAIN="$(cli sandbox ls -o json | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))' 2>/dev/null || echo '?')"
if [[ "$REMAIN" == "0" ]]; then
  pass "no sandboxes remain"
else
  fail "$REMAIN sandboxes remain"
fi

echo "==============================================================="
echo " OCI-boot smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
