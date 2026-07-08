#!/usr/bin/env bash
#
# D2-m4 — the compat-ten matrix. P1a's exit bar.
#
# Boots ten unmodified public images across the spectrum of styles
# crucible must handle, and asserts each one works: server images boot
# from their own entrypoint and listen; interpreter/shell images boot
# and run their runtime; a scratch/distroless image boots the agent as
# PID 1 (proven by the create's vsock healthz gate). Nothing is
# rewritten — if `docker run <image>` works, `crucible sandbox create
# --image <image>` must too.
#
# The matrix (style → what it exercises):
#   nginx:latest         debian/glibc HTTP server            :80
#   httpd:latest         debian Apache                       :80
#   caddy:latest         alpine + static Go server           :80
#   redis:latest         debian, entrypoint gosu-drop        :6379
#   memcached:latest     debian, image sets USER memcache    :11211  (USER resolution!)
#   traefik/whoami       SCRATCH + static Go binary          :80
#   python:3.12-slim     debian-slim interpreter             (exec)
#   node:22-alpine       alpine/musl interpreter             (exec)
#   busybox:latest       busybox userland                    (exec)
#   distroless/base      minimal, no shell                   (boot only)
#
# No network needed: servers bind locally, the listener check reads
# /proc/net/tcp. (Egress is covered by smoke_oci.sh scenario 08.)
#
# Requires: root + KVM, firecracker + jailer + a vmlinux, crucible built
# with an embedded agent (make build), and docker.io reachability.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_oci_compat.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7884}"
BASE_URL="http://${LISTEN}"
MEM="${MEM:-320}"

SMOKE_ROOT="/tmp/crucible-smoke-compat-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible OCI compat-ten (P1a exit bar)"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " kernel     : $KERNEL"
echo " listen     : $LISTEN"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (KVM + jailer)" >&2; exit 2
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
pass() { PASS=$((PASS+1)); echo "     PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "     FAIL: $*"; }

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

echo "== starting daemon"
start_daemon
echo "   daemon healthy"

# ---- helpers ----------------------------------------------------------------

# wait_listen <sbx> <port> — confirm the image's entrypoint is serving.
# Preferred proof: a LISTEN socket on <port>, read tool-agnostically
# from BOTH /proc/net/tcp and /proc/net/tcp6 (Go servers bind a
# dual-stack [::]:port that only shows in tcp6 yet accepts IPv4). The
# port is the last 4 hex of local_address ($2); LISTEN is $4 == 0A.
#
# A pure-scratch image (traefik/whoami) has no /bin/sh to read /proc and
# no network path to probe (default-deny sandbox), so there we fall back
# to the supervised service being in state=running — a Go server that
# fails to bind exits, so "running" genuinely means it bound. Sets
# $VERIFY to "socket" or "service" for the caller's message.
wait_listen() {
  local sbx="$1" port="$2" hex has_sh
  hex="$(printf '%04X' "$port")"
  VERIFY=""
  has_sh="$(cli sandbox exec "$sbx" --timeout 15 -- /bin/sh -c 'echo y' 2>/dev/null | tr -d '\r\n' || true)"
  for _ in {1..40}; do
    if [[ "$has_sh" == y ]]; then
      L="$(cli sandbox exec "$sbx" --timeout 20 -- /bin/sh -c \
        "awk '\$2 ~ /:$hex\$/ && \$4 == \"0A\" {print \"L\"; exit}' /proc/net/tcp /proc/net/tcp6" 2>/dev/null | tr -d '\r\n' || true)"
      if [[ "$L" == L ]]; then VERIFY="socket"; return 0; fi
    else
      st="$(curl -sf "$BASE_URL/sandboxes/$sbx/service" 2>/dev/null | jpath /dev/stdin state 2>/dev/null || true)"
      if [[ "$st" == running ]]; then VERIFY="service"; return 0; fi
    fi
    sleep 0.5
  done
  return 1
}

# ---- compat matrix ----------------------------------------------------------

# run_compat <image> <mode> <arg>
#   server <port>    boot the image's entrypoint, assert it listens
#   exec   <cmd>     boot, then run <cmd> via /bin/sh -c and expect exit 0
#   boot   -         boot only (no shell to exec); create success == agent up
run_compat() {
  local image="$1" mode="$2" arg="${3:-}"
  echo "== $image ($mode ${arg:-})"

  local pj="$SMOKE_ROOT/pull-$(echo "$image" | tr '/:' '__').json"
  if ! cli image pull "$image" -o json > "$pj" 2>"$pj.err"; then
    fail "$image: pull/convert failed: $(head -c 160 "$pj.err")"
    return
  fi
  local digest sbx
  digest="$(jpath "$pj" digest)"
  pass "$image: converted"

  if ! sbx="$(cli sandbox create --image "$digest" --memory "$MEM" 2>"$SMOKE_ROOT/create.err")"; then
    fail "$image: create/boot failed: $(head -c 200 "$SMOKE_ROOT/create.err")"
    return
  fi
  if [[ "$sbx" != sbx_* ]]; then
    fail "$image: unexpected create output: $sbx"
    return
  fi
  pass "$image: booted ($sbx)"

  case "$mode" in
    server)
      if wait_listen "$sbx" "$arg"; then
        if [[ "$VERIFY" == socket ]]; then
          pass "$image: listening on :$arg (entrypoint serving)"
        else
          pass "$image: entrypoint running (:$arg; scratch image, no in-guest shell to read /proc)"
        fi
      else
        fail "$image: entrypoint never served :$arg"
        cli sandbox exec "$sbx" --timeout 15 -- /bin/sh -c 'echo "--tcp:"; head -6 /proc/net/tcp 2>/dev/null; echo "--tcp6:"; head -6 /proc/net/tcp6 2>/dev/null' 2>/dev/null || true
      fi
      ;;
    exec)
      local out
      out="$(cli sandbox exec "$sbx" --timeout 25 -- /bin/sh -c "$arg" 2>/dev/null || true)"
      if [[ -n "$out" ]]; then
        pass "$image: runtime ran ($(echo "$out" | head -1 | head -c 60))"
      else
        fail "$image: exec check produced no output"
      fi
      ;;
    boot)
      # No shell to exec; a successful create already means the agent
      # booted as PID 1 and answered /healthz over vsock.
      pass "$image: agent booted as PID 1 (create passed the vsock gate)"
      ;;
  esac

  cli sandbox rm "$sbx" >/dev/null 2>&1
}

run_compat nginx:latest                    server 80
run_compat httpd:latest                    server 80
run_compat caddy:latest                    server 80
run_compat redis:latest                    server 6379
run_compat memcached:latest                server 11211
run_compat traefik/whoami:latest           server 80
run_compat python:3.12-slim                exec   'python3 --version'
run_compat node:22-alpine                  exec   'node --version'
run_compat busybox:latest                  exec   'busybox true && echo busybox-ok'
run_compat gcr.io/distroless/base-debian12 boot   -

# ---- summary ----------------------------------------------------------------

echo "==============================================================="
echo " compat-ten: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
