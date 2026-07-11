#!/usr/bin/env bash
#
# Host port publish smoke.
#
# Builds a local image serving distinctive content, side-loads it via
# the docker-save import path, publishes a host port, and curls it FROM
# THE HOST — proving ingress reaches the service inside the microVM.
#
# Scenarios:
#   01  daemon up with networking (port publish rides on the net layer)
#   02  build + import the local test image (CRUCIBLE-TESTAPP-OK on :80)
#   03  create --publish 8080:80 (no --net-allow → egress-denied NIC)
#   04  curl localhost:8080 from the host → CRUCIBLE-TESTAPP-OK  (ingress!)
#   05  a second sandbox on a different host port serves independently
#   06  delete releases the host port (re-publish on the same port works)
#   07  --publish 127.0.0.1:PORT:80 binds localhost-only
#   08  fork -p exposes a forked copy of a running server on its own port
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built
# (make build), docker (to build/save the test image), and a host with a
# default route (networking needs --network-egress-iface).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_testapp.sh

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

# SMOKE_ROOT holds the image cache + per-sandbox rootfs copies. On a
# filesystem without reflink each sandbox is a FULL rootfs byte-copy, so
# point SMOKE_BASE at a roomy on-disk path if /tmp is a small tmpfs.
SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-testapp-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible host port publish smoke"
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
command -v docker >/dev/null || { echo "error: docker needed to build/save the test image" >&2; exit 2; }
command -v curl   >/dev/null || { echo "error: curl needed" >&2; exit 2; }

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE (networking is required for publish)" >&2; exit 2; }

jpath() { local f="$1"; shift; python3 -c "
import json,sys
d=json.load(open('$f'))
for k in sys.argv[1:]: d = d[int(k)] if k.isdigit() else d[k]
print(d)" "$@"; }

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

echo "== 01 starting daemon (networking enabled)"
start_daemon
pass "daemon healthy with networking + image support"

# ---- 02 build + import the test image ---------------------------------------

echo "== 02 build + import the local test image"
if ! docker build -q -t crucible-testapp "$HERE/testapp" >/dev/null 2>"$SMOKE_ROOT/build.err"; then
  fail "docker build: $(cat "$SMOKE_ROOT/build.err")"; exit 1
fi
if docker save crucible-testapp | cli image import -o json > "$SMOKE_ROOT/img.json" 2>"$SMOKE_ROOT/import.err"; then
  DIGEST="$(jpath "$SMOKE_ROOT/img.json" digest)"
  pass "imported test image ($DIGEST)"
else
  fail "image import: $(cat "$SMOKE_ROOT/import.err")"; exit 1
fi

# hit_host <port> — curl the published port from the host, expect our body.
hit_host() {
  local url="$1" body
  for _ in {1..30}; do
    body="$(curl -s --max-time 3 "$url" 2>/dev/null || true)"
    [[ "$body" == *CRUCIBLE-TESTAPP-OK* ]] && { echo "$body"; return 0; }
    sleep 0.5
  done
  return 1
}

# diagnose <sandbox-id> — on a publish failure, isolate whether it's the
# nft return path, L3 reachability, the guest server, or the forwarder.
# Dials the guest IP DIRECTLY (bypassing the forwarder) and dumps state.
diagnose() {
  local id="$1" gip
  echo "   --- diagnostics for $id ---"
  gip="$(cli sandbox inspect "$id" 2>/dev/null |
    python3 -c 'import json,sys; d=json.load(sys.stdin); print((d.get("network") or {}).get("guest_ip",""))' 2>/dev/null)"
  echo "   guest_ip: ${gip:-<none>}"
  if [[ -n "$gip" ]]; then
    echo "   ip route get $gip:"; ip route get "$gip" 2>&1 | sed 's/^/     /'
    echo "   direct curl http://$gip:80/ (bypasses forwarder):"
    curl -s --max-time 4 "http://$gip:80/" 2>&1 | sed 's/^/     /' || echo "     (direct dial failed)"
  fi
  echo "   nft input chain:"
  nft list chain inet crucible input 2>&1 | sed 's/^/     /'
  echo "   host listeners on 8080:"; ss -ltnp 2>/dev/null | grep -E ':8080' | sed 's/^/     /' || echo "     (none)"
  echo "   --- end diagnostics ---"
}

# ---- 03 + 04 publish and hit from the host ----------------------------------

echo "== 03 create --publish 8080:80 (egress-denied NIC, no --net-allow)"
if SBX="$(cli sandbox create --image "$DIGEST" --memory 256 --publish 8080:80)" && [[ "$SBX" == sbx_* ]]; then
  pass "created $SBX with published port 8080:80"
else
  fail "create --publish failed; daemon log:"; tail -30 "$DAEMON_LOG"; exit 1
fi

echo "== 04 curl the published port FROM THE HOST"
if hit_host "http://localhost:8080/" >/dev/null; then
  pass "curl localhost:8080 → CRUCIBLE-TESTAPP-OK (ingress reached the microVM)"
else
  fail "host could not reach the published port"; diagnose "$SBX"; tail -20 "$DAEMON_LOG"
fi

# ---- 05 second sandbox, different port --------------------------------------

echo "== 05 a second sandbox on a different host port"
if SBX2="$(cli sandbox create --image "$DIGEST" --memory 256 --publish 8081:80)" && [[ "$SBX2" == sbx_* ]]; then
  if hit_host "http://localhost:8081/" >/dev/null; then
    pass "second sandbox serves on :8081 independently"
  else
    fail "second sandbox :8081 unreachable"
  fi
else
  fail "second create --publish failed"
fi

# ---- 06 delete releases the host port ---------------------------------------

echo "== 06 delete frees the host port"
cli sandbox rm "$SBX" >/dev/null 2>&1
sleep 1
if SBX3="$(cli sandbox create --image "$DIGEST" --memory 256 --publish 8080:80)" && [[ "$SBX3" == sbx_* ]]; then
  if hit_host "http://localhost:8080/" >/dev/null; then
    pass "re-published :8080 after delete (host port was released)"
  else
    fail "re-published :8080 unreachable"
  fi
  cli sandbox rm "$SBX3" >/dev/null 2>&1
else
  fail "re-publish on :8080 after delete failed (port not released?)"
fi

# ---- 07 localhost-only bind -------------------------------------------------

echo "== 07 --publish 127.0.0.1:PORT binds localhost-only"
if SBX4="$(cli sandbox create --image "$DIGEST" --memory 256 --publish 127.0.0.1:8082:80)" && [[ "$SBX4" == sbx_* ]]; then
  if hit_host "http://127.0.0.1:8082/" >/dev/null; then
    pass "localhost-pinned publish reachable on 127.0.0.1:8082"
  else
    fail "localhost-pinned publish unreachable"
  fi
  cli sandbox rm "$SBX4" >/dev/null 2>&1
else
  fail "localhost-pinned create failed"
fi

# ---- 08 fork -p publishes the fork on its own host port ---------------------

echo "== 08 fork -p 8083:80 exposes the forked copy"
if [[ -n "${SBX4:-}" ]]; then
  if SNP="$(cli snapshot create "$SBX4")" && [[ "$SNP" == snap_* ]] &&
     FORK="$(cli fork "$SNP" -p 8083:80)" && [[ "$FORK" == sbx_* ]]; then
    if hit_host "http://localhost:8083/" >/dev/null; then
      pass "fork of a running server reachable on its own port 8083"
    else
      fail "fork published on 8083 but unreachable"
    fi
    cli sandbox rm "$FORK" >/dev/null 2>&1
  else
    fail "snapshot+fork -p failed (snap=$SNP fork=${FORK:-})"
  fi
else
  fail "no SBX4 to fork (check 07 failed earlier)"
fi

# ---- cleanup + summary ------------------------------------------------------

for id in "${SBX2:-}"; do [[ -n "$id" ]] && cli sandbox rm "$id" >/dev/null 2>&1; done

echo "==============================================================="
echo " smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
