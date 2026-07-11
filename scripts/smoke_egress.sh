#!/usr/bin/env bash
#
# Egress-model smoke (v0.4.1 / A6): the range-based egress modes and — the whole
# point — the SSRF tripwire that guards them.
#
#   01  daemon healthy
#   02  default-deny: a no-network sandbox reaches nothing
#   03  full-egress: reaches a public host (1.1.1.1:443) ...
#   04  ... but is STILL refused cloud metadata (169.254.169.254) AND RFC1918
#       (the SSRF-regression tripwire — public-hosts-only, no exceptions)
#   05  CIDR: --net-allow-cidr reaches an in-range public IP; out-of-range and a
#       private-range CIDR reach nothing
#   06  policy: a scoped token WITHOUT net_full_egress cannot use --net-full-egress
#
# Reachability is a raw TCP connect from inside the guest (busybox nc), so it
# does not depend on the target speaking HTTP. 1.1.1.1:443 (Cloudflare) is the
# public target; every "must be blocked" check asserts the connect FAILS.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built with an
# embedded agent (make build), curl, and outbound internet on the host.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_egress.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7893}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-alpine:latest}"

PUBLIC_HOST="${PUBLIC_HOST:-1.1.1.1}"; PUBLIC_PORT="${PUBLIC_PORT:-443}"
OTHER_PUBLIC="${OTHER_PUBLIC:-8.8.8.8}"        # public, but outside the CIDR test range
METADATA="169.254.169.254"                     # cloud metadata (link-local)
RFC1918="10.255.255.1"                         # private

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-egress-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"; LOG_DIR="$SMOKE_ROOT/logs"
TOKEN_FILE="$SMOKE_ROOT/tokens.json"; DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible egress-model smoke (A6)"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " public tgt : $PUBLIC_HOST:$PUBLIC_PORT"
echo "==============================================================="

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }; done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]] || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }

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
    "$@" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}
stop_daemon() { [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null; [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null; DAEMON_PID=""; }
cleanup() {
  for id in $(curl -sf "$BASE_URL/sandboxes" 2>/dev/null | grep -o 'sbx_[a-z0-9]*' | sort -u); do
    curl -sf -X DELETE "$BASE_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  stop_daemon
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$SMOKE_ROOT"
}
trap cleanup EXIT

# reachable <sbx> <host> <port> — 0 if the guest can TCP-connect, else non-zero.
reachable() {
  cli sandbox exec "$1" -- sh -c "nc -w 4 $2 $3 </dev/null >/dev/null 2>&1"
}
# mksbx <flags...> — create an alpine sandbox, print its id (or empty on failure).
mksbx() { cli sandbox create --image "$IMAGE" --memory 256 "$@" 2>/dev/null; }

echo "== 01 starting daemon"
start_daemon
pass "daemon healthy"

# ---- 02 default-deny baseline ----------------------------------------------
echo "== 02 default-deny: a no-network sandbox reaches nothing"
S0="$(mksbx)"
if [[ "$S0" == sbx_* ]]; then
  if reachable "$S0" "$PUBLIC_HOST" "$PUBLIC_PORT"; then
    fail "no-network sandbox reached $PUBLIC_HOST (default-deny broken!)"
  else
    pass "no-network sandbox cannot reach $PUBLIC_HOST"
  fi
else fail "create baseline sandbox failed"; fi

# ---- 03/04 full-egress + the SSRF tripwire ---------------------------------
echo "== 03/04 full-egress reaches public, refuses metadata + RFC1918"
SF="$(mksbx --net-full-egress)"
if [[ "$SF" == sbx_* ]]; then
  if reachable "$SF" "$PUBLIC_HOST" "$PUBLIC_PORT"; then
    pass "full-egress reached $PUBLIC_HOST:$PUBLIC_PORT"
  else
    fail "full-egress could NOT reach $PUBLIC_HOST:$PUBLIC_PORT (is the host online?)"
  fi
  # THE TRIPWIRE — these MUST fail even under full-egress.
  if reachable "$SF" "$METADATA" 80; then
    fail "SSRF: full-egress reached cloud metadata $METADATA — guard regressed!"
  else
    pass "full-egress refused cloud metadata $METADATA"
  fi
  if reachable "$SF" "$RFC1918" 80; then
    fail "SSRF: full-egress reached RFC1918 $RFC1918 — guard regressed!"
  else
    pass "full-egress refused RFC1918 $RFC1918"
  fi
else fail "create full-egress sandbox failed"; fi

# ---- 05 CIDR allowlist ------------------------------------------------------
echo "== 05 CIDR: in-range public reachable; out-of-range + private-CIDR are not"
PUB24="$(printf '%s' "$PUBLIC_HOST" | sed 's/\.[0-9]*$/.0\/24/')"   # e.g. 1.1.1.0/24
SC="$(mksbx --net-allow-cidr "$PUB24")"
if [[ "$SC" == sbx_* ]]; then
  reachable "$SC" "$PUBLIC_HOST" "$PUBLIC_PORT" && pass "CIDR $PUB24 reached $PUBLIC_HOST" \
    || fail "CIDR $PUB24 did not reach $PUBLIC_HOST"
  reachable "$SC" "$OTHER_PUBLIC" "$PUBLIC_PORT" && fail "out-of-range $OTHER_PUBLIC reachable (CIDR too broad)" \
    || pass "out-of-range $OTHER_PUBLIC not reachable"
  reachable "$SC" "$METADATA" 80 && fail "SSRF: CIDR sandbox reached metadata" \
    || pass "CIDR sandbox refused metadata"
else fail "create CIDR sandbox failed"; fi

SP="$(mksbx --net-allow-cidr 10.0.0.0/8)"   # wholly-private CIDR = no-op
if [[ "$SP" == sbx_* ]]; then
  reachable "$SP" "$RFC1918" 80 && fail "private-range CIDR reached $RFC1918 (should be a no-op)" \
    || pass "wholly-private CIDR reaches nothing (no-op, public-only invariant)"
else fail "create private-CIDR sandbox failed"; fi

# ---- 06 policy ceiling ------------------------------------------------------
echo "== 06 a scoped token without net_full_egress cannot use --net-full-egress"
stop_daemon
printf '%s' '{"operations":["create","exec","delete"]}' > "$SMOKE_ROOT/scoped.json"
SKEY="$("$CRUCIBLE_BIN" daemon token add --token-file "$TOKEN_FILE" --name scoped --policy "$SMOKE_ROOT/scoped.json" | grep -o 'crucible_[A-Za-z0-9_-]*')"
[[ -n "$SKEY" ]] || { echo "error: could not mint scoped token" >&2; exit 4; }
start_daemon --token-file "$TOKEN_FILE"
OUT="$("$CRUCIBLE_BIN" --addr "$LISTEN" --token "$SKEY" sandbox create --image "$IMAGE" --net-full-egress 2>&1)"
if [[ "$OUT" == sbx_* ]]; then
  fail "scoped token created a full-egress sandbox (policy gate missing!)"
else
  pass "scoped token rejected for --net-full-egress ($(printf '%s' "$OUT" | head -1))"
fi

echo "==============================================================="
echo " egress smoke: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
