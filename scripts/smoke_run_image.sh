#!/usr/bin/env bash
#
# One-command image run smoke.
#
# Proves `crucible sandbox create --image <ref>` Just Works like
# `docker run` — no separate pull/import/parse-digest step:
#
#   02  registry one-liner: --image nginx:alpine → daemon pulls, boots, serves
#   03  local one-liner:    --image <local docker tag> → CLI auto-saves + boots
#   04  --pull never on an uncached ref fails cleanly (no network)
#   05  a store hit re-creates without re-pulling
#
# Scenario 02 needs internet; it is skipped (not failed) when offline.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built
# (make build), docker, curl, and a host with a default route.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_run_image.sh

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
REGISTRY_IMAGE="${REGISTRY_IMAGE:-nginx:alpine}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-run-image-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible one-command image run smoke"
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
command -v docker >/dev/null || { echo "error: docker needed" >&2; exit 2; }
command -v curl   >/dev/null || { echo "error: curl needed" >&2; exit 2; }

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0; SKIP=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
skip() { SKIP=$((SKIP+1)); echo "   SKIP: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

have_internet() { timeout 5 bash -c 'exec 3<>/dev/tcp/registry-1.docker.io/443' 2>/dev/null; }

# ---- daemon -----------------------------------------------------------------

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

# hit <url> <needle> — curl until the body contains needle (or give up).
hit() {
  local url="$1" needle="$2" body
  for _ in {1..30}; do
    body="$(curl -s --max-time 3 "$url" 2>/dev/null || true)"
    [[ "$body" == *"$needle"* ]] && return 0
    sleep 0.5
  done
  return 1
}

echo "== 01 starting daemon (networking + images)"
start_daemon
pass "daemon healthy"

# ---- 02 registry one-liner (daemon pulls) -----------------------------------

echo "== 02 registry one-liner: create --image $REGISTRY_IMAGE --pull always"
if ! have_internet; then
  skip "no registry connectivity — skipping the daemon-pull scenario"
else
  # --pull always forces the daemon-side registry pull (bypasses any
  # local docker copy), proving the pure `docker run`-style path.
  if SBX="$(cli sandbox create --image "$REGISTRY_IMAGE" --pull always --memory 256 --publish 8080:80)" && [[ "$SBX" == sbx_* ]]; then
    if hit "http://localhost:8080/" "nginx"; then
      pass "one command pulled + booted + served $REGISTRY_IMAGE (curl localhost:8080)"
    else
      fail "published $REGISTRY_IMAGE unreachable"; tail -20 "$DAEMON_LOG"
    fi
  else
    fail "create --image $REGISTRY_IMAGE (registry pull) failed"; tail -30 "$DAEMON_LOG"
  fi
fi

# ---- 03 local one-liner (CLI auto-saves the local docker tag) ----------------

echo "== 03 local one-liner: build a tag, then create --image <tag> (no explicit import)"
if ! docker build -q -t crucible-testapp "$HERE/testapp" >/dev/null 2>"$SMOKE_ROOT/build.err"; then
  fail "docker build: $(cat "$SMOKE_ROOT/build.err")"
else
  if SBX3="$(cli sandbox create --image crucible-testapp --memory 256 --publish 8081:80)" && [[ "$SBX3" == sbx_* ]]; then
    if hit "http://localhost:8081/" "CRUCIBLE-TESTAPP-OK"; then
      pass "one command auto-saved the local tag + booted + served (curl localhost:8081)"
    else
      fail "published local-tag sandbox unreachable"; tail -20 "$DAEMON_LOG"
    fi
  else
    fail "create --image crucible-testapp (local docker tag) failed"; tail -30 "$DAEMON_LOG"
  fi
fi

# ---- 04 --pull never on an uncached ref fails cleanly ------------------------

echo "== 04 --pull never on an uncached ref is a clean error (no network)"
if out="$(cli sandbox create --image crucible-uncached-xyz:latest --pull never --memory 256 2>&1)"; then
  fail "create --pull never on a miss unexpectedly succeeded: $out"
else
  if [[ "$out" == *"pull"* || "$out" == *"not"* ]]; then
    pass "--pull never on a store miss rejected cleanly"
  else
    fail "unexpected error text: $out"
  fi
fi

# ---- 05 store hit re-creates without a re-pull -------------------------------

echo "== 05 a store hit re-creates the local image (already converted)"
if SBX5="$(cli sandbox create --image crucible-testapp --memory 256 --publish 8082:80)" && [[ "$SBX5" == sbx_* ]]; then
  if hit "http://localhost:8082/" "CRUCIBLE-TESTAPP-OK"; then
    pass "second create from the same image serves on :8082"
  else
    fail "second sandbox unreachable"
  fi
else
  fail "store-hit re-create failed"
fi

# ---- summary ----------------------------------------------------------------

echo "==============================================================="
echo " image-run smoke: $PASS passed, $FAIL failed, $SKIP skipped"
echo " transcripts: $SMOKE_ROOT"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
