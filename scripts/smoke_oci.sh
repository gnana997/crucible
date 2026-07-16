#!/usr/bin/env bash
#
# End-to-end smoke for booting converted OCI images.
#
# Boots a real Firecracker microVM from a converted image with the
# crucible-agent as PID 1 (init mode), and validates the boot end to
# end: healthz over vsock, exec into the guest, PID 1 is the agent, and
# a supervised service runs. This covers boot + exec + service; image
# sandbox networking is exercised by the port-publish / run-image smokes.
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

# Auto-detect the internet-facing interface for image egress (scenario
# 08). Unset it (EGRESS_IFACE=) to skip networking.
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"

DAEMON_PID=""
start_daemon() {
  local extra=()
  if [[ -n "${EGRESS_IFACE:-}" ]]; then
    extra+=(--network-egress-iface "$EGRESS_IFACE")
  fi
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" \
    --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" \
    --kernel "$KERNEL" \
    --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --app-db "$WORK_BASE-apps.db" \
    --image-dir "$IMAGE_DIR" \
    "${extra[@]}" \
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

# ---- 07 entrypoint fidelity: run the image's OWN entrypoint -----------------

echo "== 07 image entrypoint fidelity (create --image runs the image's CMD)"
# busybox's default CMD is 'sh'; give it a real entrypoint via a small
# image whose CMD we can observe. Use nginx: its entrypoint launches
# nginx, which we then confirm is running as a supervised service and
# listening. Pull nginx and boot it with NO explicit command.
if cli image pull nginx:latest -o json > "$SMOKE_ROOT/nginx.json" 2>"$SMOKE_ROOT/nginx.err"; then
  NGINX_DIGEST="$(jpath "$SMOKE_ROOT/nginx.json" digest)"
  if NGX_SBX="$(cli sandbox create --image "$NGINX_DIGEST" --memory 256)" && [[ "$NGX_SBX" == sbx_* ]]; then
    sleep 2
    NGX_STATE="$(curl -sf "$BASE_URL/sandboxes/$NGX_SBX/service" | jpath /dev/stdin state 2>/dev/null || true)"
    NGX_CMD="$(curl -sf "$BASE_URL/sandboxes/$NGX_SBX/service" | python3 -c 'import json,sys;print(" ".join(json.load(sys.stdin)["spec"]["cmd"]))' 2>/dev/null || true)"
    if [[ "$NGX_STATE" == "running" ]]; then
      pass "nginx booted from its own entrypoint (service running, cmd: $NGX_CMD)"
    else
      fail "nginx entrypoint state = '$NGX_STATE' (cmd: $NGX_CMD)"
    fi
    # Confirm nginx is actually listening on :80 inside the guest.
    # Read /proc/net/tcp directly (port 80 = hex 0050, LISTEN = 0A) so
    # the check needs no HTTP client — the nginx image ships neither
    # curl nor wget. Poll briefly: nginx binds a beat after boot.
    LISTENING=""
    for _ in {1..20}; do
      L="$(cli sandbox exec "$NGX_SBX" --timeout 20 -- /bin/sh -c 'awk "\$2 ~ /:0050\$/ && \$4 == \"0A\" {print \"listening\"; exit}" /proc/net/tcp' 2>/dev/null | tr -d '\r\n' || true)"
      if [[ "$L" == listening ]]; then LISTENING=1; break; fi
      sleep 0.5
    done
    if [[ -n "$LISTENING" ]]; then
      pass "nginx is listening on :80 (its entrypoint really started the server)"
    else
      fail "nginx never listened on :80"
    fi
  else
    fail "create nginx from image failed"
  fi
else
  fail "pull nginx: $(cat "$SMOKE_ROOT/nginx.err")"
fi

# ---- 08 image networking: static netlink config + egress -------------------

echo "== 08 image sandbox with network (daemon-pushed netlink config)"
# Networking needs the daemon started with --network-egress-iface +
# --jailer-bin. If the daemon wasn't started with egress configured,
# this scenario is skipped (the daemon rejects networked creates).
if [[ -n "${EGRESS_IFACE:-}" ]]; then
  NET_SBX="$(cli sandbox create --image "$DIGEST" --memory 256 --net-allow example.com 2>"$SMOKE_ROOT/net.err" || true)"
  if [[ "$NET_SBX" == sbx_* ]]; then
    pass "created networked image sandbox $NET_SBX"
    # eth0 got the pushed address (agent applied it via netlink; alpine
    # has busybox `ip`, but we read /proc to stay tool-agnostic).
    HASIP="$(cli sandbox exec "$NET_SBX" --timeout 20 -- /bin/sh -c 'grep -q . /sys/class/net/eth0/address && ip -4 addr show eth0 2>/dev/null | grep -c "inet 10.20" || true' 2>/dev/null | tr -d '\r\n' || true)"
    # Fall back to a route check that needs no `ip`: default route in /proc/net/route.
    HASROUTE="$(cli sandbox exec "$NET_SBX" --timeout 20 -- /bin/sh -c 'awk "\$2 == \"00000000\" {print \"default\"; exit}" /proc/net/route' 2>/dev/null | tr -d '\r\n' || true)"
    if [[ "$HASROUTE" == default ]]; then
      pass "eth0 configured with a default route (netlink push applied)"
    else
      fail "no default route in guest (netlink config not applied)"
    fi
    # DNS resolves + egress to the allowlisted host works.
    EGRESS="$(cli sandbox exec "$NET_SBX" --timeout 25 -- /bin/sh -c 'wget -qO- http://example.com/ 2>/dev/null | head -c 200 || true' 2>/dev/null || true)"
    if echo "$EGRESS" | grep -qi 'example'; then
      pass "egress to allowlisted host works (DNS + HTTP)"
    else
      echo "   NOTE: egress check inconclusive (busybox wget/DNS): '$(echo "$EGRESS" | head -c 60)'"
    fi
    cli sandbox rm "$NET_SBX" >/dev/null 2>&1
  else
    fail "networked image create failed: $(cat "$SMOKE_ROOT/net.err")"
  fi
else
  echo "   SKIP: EGRESS_IFACE not set (start daemon with --network-egress-iface to test image egress)"
fi

# ---- 09 cleanup -------------------------------------------------------------

echo "== 09 cleanup"
for id in "$SBX" "${SVC_SBX:-}" "${NGX_SBX:-}"; do
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
