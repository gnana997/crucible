#!/usr/bin/env bash
#
# Ingress-proxy + inbound-isolation smoke (v0.4.2).
#
#   01  daemon healthy with the proxy enabled
#   02  reach an app BY NAME through the proxy (Host header → current instance)
#   03  self-heal: kill the instance → the proxy follows the app to the new one
#       (the resolver never routes a stale IP)
#   04  unknown host → 404; an app with no ready instance → 502 (no buffering)
#   05  inbound isolation: a guest cannot reach a PEER guest's IP — the
#       proxy does not open a lateral-movement path
#
# The proxy dials the guest from the daemon's host netns (same origin as the
# port-publish forwarder), so inbound reaches a guest only via the proxy /
# published ports; peers stay unreachable because egress is default-deny and
# RFC1918 is blocked.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built with an
# embedded agent (make build), curl, network to pull the image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_proxy.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7894}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
PROXY_PORT="${PROXY_PORT:-8888}"
DOMAIN="${DOMAIN:-apps.local}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-proxy-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"; LOG_DIR="$SMOKE_ROOT/logs"
APP_DB="$SMOKE_ROOT/apps.db"; DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible ingress-proxy smoke (v0.4.2)"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " proxy      : http://127.0.0.1:$PROXY_PORT (domain: $DOMAIN)"
echo "==============================================================="

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }; done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]] || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }

# The daemon must be new enough to have the proxy (v0.4.2).
if ! LC_ALL=C grep -qa "ingress proxy enabled\|proxy-listen" "$CRUCIBLE_BIN"; then
  echo "error: $CRUCIBLE_BIN predates the ingress proxy (v0.4.2). Rebuild: make build" >&2
  exit 2
fi

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
api()  { curl -s "$@"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

# hitproxy <host> <needle> — curl the proxy with a Host header until the body
# contains needle (or give up). Returns 0 on match.
hitproxy() {
  local host="$1" needle="$2" body
  for _ in $(seq 1 40); do
    body="$(curl -s --max-time 3 -H "Host: $host" "http://127.0.0.1:$PROXY_PORT/" 2>/dev/null || true)"
    [[ "$body" == *"$needle"* ]] && return 0
    sleep 0.5
  done
  return 1
}
proxycode() { # <host> — print the HTTP status the proxy returns
  curl -s -o /dev/null -w '%{http_code}' --max-time 4 -H "Host: $1" "http://127.0.0.1:$PROXY_PORT/" 2>/dev/null
}

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --network-egress-iface "$EGRESS_IFACE" \
    --proxy-listen ":$PROXY_PORT" --proxy-domain "$DOMAIN" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}
cleanup() {
  cli app rm web >/dev/null 2>&1 || true
  cli app rm dormant >/dev/null 2>&1 || true
  for id in $(api "$BASE_URL/sandboxes" 2>/dev/null | grep -o 'sbx_[a-z0-9]*' | sort -u); do
    api -X DELETE "$BASE_URL/sandboxes/$id" >/dev/null 2>&1 || true
  done
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null
  [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$SMOKE_ROOT"
}
trap cleanup EXIT

echo "== 01 starting daemon (proxy enabled)"
start_daemon
if grep -qa '"msg":"ingress proxy enabled"' "$DAEMON_LOG"; then
  pass "daemon healthy, ingress proxy enabled"
else
  fail "proxy did not enable (see $DAEMON_LOG)"; tail -20 "$DAEMON_LOG"
fi

# ---- 02 reach an app by name through the proxy ------------------------------
echo "== 02 reach app 'web' BY NAME through the proxy"
cli app rm web >/dev/null 2>&1 || true
if [[ "$(cli app create web --image "$IMAGE" --port 80 --restart always --memory 256 2>/dev/null)" == "web" ]]; then
  if hitproxy "web.$DOMAIN" "html" || hitproxy "web.$DOMAIN" "nginx"; then
    pass "proxy routed web.$DOMAIN → the app's current instance"
  else
    fail "proxy did not route web.$DOMAIN"; tail -30 "$DAEMON_LOG"
  fi
else
  fail "app create web failed"; tail -30 "$DAEMON_LOG"
fi

# ---- 03 self-heal: the proxy follows the app to a new instance --------------
echo "== 03 kill the instance → proxy follows the app to the fresh one"
INST="$(api "$BASE_URL/apps/web" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
if [[ "$INST" == sbx_* ]]; then
  cli sandbox rm "$INST" >/dev/null 2>&1 || true
  HEALED=0
  for _ in $(seq 1 60); do
    NEW="$(api "$BASE_URL/apps/web" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
    if [[ "$NEW" == sbx_* && "$NEW" != "$INST" ]] && (hitproxy "web.$DOMAIN" "html" || hitproxy "web.$DOMAIN" "nginx"); then
      HEALED=1; break
    fi
    sleep 1
  done
  [[ "$HEALED" -eq 1 ]] && pass "proxy re-resolved to the new instance ($INST → $NEW) — no stale route" \
    || fail "proxy did not follow the app after self-heal ($INST → ${NEW:-none})"
else
  fail "could not read web's instance id"
fi

# ---- 04 no-route / no-instance status ---------------------------------------
echo "== 04 unknown host → 404; app with no ready instance → 502"
code="$(proxycode "nope.$DOMAIN")"
[[ "$code" == "404" ]] && pass "unknown host → 404" || fail "unknown host → $code, want 404"

cli app rm dormant >/dev/null 2>&1 || true
cli app create dormant --image "$IMAGE" --port 80 --stopped >/dev/null 2>&1
code="$(proxycode "dormant.$DOMAIN")"
[[ "$code" == "502" ]] && pass "app with no ready instance → 502" || fail "no-instance → $code, want 502"
cli app rm dormant >/dev/null 2>&1 || true

# ---- 05 inbound isolation: a guest can't reach a peer guest's IP ------------
echo "== 05 inbound isolation: web's guest cannot reach a peer guest's IP"
PEER="$(cli sandbox create --image "$IMAGE" --memory 256 --net-allow example.com 2>/dev/null)"
if [[ "$PEER" == sbx_* ]]; then
  PEER_IP="$(api "$BASE_URL/sandboxes/$PEER" 2>/dev/null | grep -o '"guest_ip":"[0-9.]*"' | grep -o '[0-9.]*' | head -1)"
  WEB_INST="$(api "$BASE_URL/apps/web" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
  if [[ -n "$PEER_IP" && "$WEB_INST" == sbx_* ]]; then
    if cli sandbox exec "$WEB_INST" -- sh -c "nc -w 3 $PEER_IP 80 </dev/null" >/dev/null 2>&1; then
      fail "web's guest reached peer $PEER_IP — inbound/lateral isolation broken!"
    else
      pass "web's guest cannot reach peer $PEER_IP (isolation holds with the proxy)"
    fi
  else
    fail "could not read peer IP / web instance (peer_ip=$PEER_IP web=$WEB_INST)"
  fi
  cli sandbox rm "$PEER" >/dev/null 2>&1 || true
else
  fail "create peer sandbox failed"
fi

echo "== 06 IPv6 at the edge: dual-stack proxy + bracketed v6 port publish"
# The proxy binds a wildcard ":port" (dual-stack); guests stay v4 — the proxy
# does the family hop. Skipped when the host has no v6 loopback.
if ip -6 addr show dev lo 2>/dev/null | grep -q '::1'; then
  BODY6="$(curl -sg --max-time 4 -H "Host: web.$DOMAIN" "http://[::1]:$PROXY_PORT/" 2>/dev/null || true)"
  if [[ "$BODY6" == *nginx* || "$BODY6" == *html* ]]; then
    pass "proxy serves the app over IPv6 ([::1] → v4 guest)"
  else
    fail "proxy did not serve over IPv6: '${BODY6:0:80}'"
  fi
  # A published port pinned to a v6 address ([::1]:PORT:80, docker syntax).
  V6_PORT=$((PROXY_PORT+1))
  if cli app update web --image "$IMAGE" -p "[::1]:$V6_PORT:80" --restart always \
      --health "http:80:/" --memory 256 >/dev/null 2>&1; then
    SERVED6=0
    for _ in $(seq 1 40); do
      B="$(curl -sg --max-time 3 "http://[::1]:$V6_PORT/" 2>/dev/null || true)"
      [[ "$B" == *nginx* || "$B" == *html* ]] && { SERVED6=1; break; }
      sleep 0.5
    done
    [[ "$SERVED6" -eq 1 ]] && pass "published port bound to [::1] serves over IPv6" \
      || fail "published port [::1]:$V6_PORT never served"
  else
    fail "app update with a bracketed v6 publish spec was rejected"
  fi
else
  echo "   SKIP: no IPv6 loopback on this host"
fi

cli app rm web >/dev/null 2>&1 || true

echo "==============================================================="
echo " proxy smoke: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
