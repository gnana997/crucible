#!/usr/bin/env bash
#
# Observability smoke (v0.5.4): per-app Prometheus metrics off the
# ingress proxy, plus the daemon pprof endpoint.
#
# What this proves on real KVM:
#   01  a proxy-fronted app serves, and driving traffic through the proxy produces
#       per-app request metrics on /metrics: app_requests_total{app,code} +
#       app_request_duration_seconds (with the right `app` label)
#   02  per-app lifecycle gauges appear from the app manager (pull-model):
#       app_replicas / app_ready_replicas / app_up
#   03  CARDINALITY GUARD: a request for an UNKNOWN host (404) is NOT counted —
#       no app_requests_total series for a bogus app name
#   04  daemon pprof (--pprof-listen) serves /debug/pprof/ (J9 slice)
#
# OTLP export checks arrive later.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_observability.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7901}"
PROXY_PORT="${PROXY_PORT:-7902}"
PPROF_PORT="${PPROF_PORT:-7903}"
OTLP_PORT="${OTLP_PORT:-14317}"   # a dummy OTLP endpoint (no collector) — wiring check only
DOMAIN="${DOMAIN:-apps.local}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-obs-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible observability smoke (v0.5.4)"
echo " output: $SMOKE_ROOT   proxy: 127.0.0.1:$PROXY_PORT   pprof: 127.0.0.1:$PPROF_PORT"
echo "==============================================================="

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
# This smoke starts its own daemon; a running systemd daemon holds host-global
# network singletons (anycast VIP + nft table) it would fight over.
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet crucible 2>/dev/null; then
  echo "error: stop the systemd 'crucible' daemon first (it holds host-global network state):" >&2
  echo "         sudo systemctl stop crucible && sudo $0 ; sudo systemctl start crucible" >&2
  exit 2
fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }
metrics() { curl -s --max-time 5 "$BASE_URL/metrics" 2>/dev/null; }
proxy_hit() { curl -s -o /dev/null --max-time 5 -H "Host: $1" "http://127.0.0.1:${PROXY_PORT}/" 2>/dev/null; }
serves() { for _ in {1..40}; do [[ "$(curl -s --max-time 3 -H "Host: web.${DOMAIN}" "http://127.0.0.1:${PROXY_PORT}/" 2>/dev/null)" == *nginx* ]] && return 0; sleep 0.5; done; return 1; }
# Read app status from the API (compact JSON); the CLI `app get` pretty-prints
# ("phase": "x" with a space), which a compact grep would miss.
wait_phase() { for _ in {1..80}; do [[ "$(curl -s "$BASE_URL/apps/web" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1)" == *"$1"* ]] && return 0; sleep 0.5; done; return 1; }

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --network-egress-iface "$EGRESS_IFACE" \
    --proxy-listen "127.0.0.1:$PROXY_PORT" --proxy-domain "$DOMAIN" \
    --pprof-listen "127.0.0.1:$PPROF_PORT" \
    --otlp-endpoint "http://127.0.0.1:${OTLP_PORT}" --otlp-insecure \
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
  cli app rm web >/dev/null 2>&1 || true; sleep 1
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== 00 start daemon (proxy + pprof) + app"
start_daemon
if [[ "$(cli app create web --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 2>/dev/null)" != "web" ]]; then
  fail "app create failed"; tail -30 "$DAEMON_LOG"; exit 1
fi
if serves && wait_phase running; then pass "app serves through the proxy"; else fail "app never served"; tail -30 "$DAEMON_LOG"; exit 1; fi

echo "== 01 per-app request metrics appear after driving traffic"
for _ in $(seq 1 20); do proxy_hit "web.${DOMAIN}"; done
sleep 1
M="$(metrics)"
if [[ "$M" == *'app_requests_total{app="web"'* ]]; then
  pass "app_requests_total has the web app label"
else
  fail "app_requests_total missing web label"; echo "$M" | grep -i 'app_requests' | head
fi
[[ "$M" == *'app_request_duration_seconds_count{app="web"}'* ]] \
  && pass "app_request_duration_seconds recorded for web" \
  || fail "app_request_duration_seconds missing for web"

echo "== 02 per-app lifecycle gauges (pull-model)"
for want in 'app_up{app="web"} 1' 'app_replicas{app="web"}' 'app_ready_replicas{app="web"}'; do
  [[ "$M" == *"$want"* ]] && pass "gauge present: $want" || fail "gauge missing: $want"
done

echo "== 03 cardinality guard: an unknown host is not counted"
proxy_hit "totally-bogus-$RANDOM.${DOMAIN}" >/dev/null 2>&1
sleep 1
if metrics | grep -q 'app_requests_total{app="totally-bogus'; then
  fail "SECURITY/cardinality: an unknown host produced a per-app series"
else
  pass "unknown host did not create an app_requests_total series (bounded cardinality)"
fi

echo "== 04 daemon pprof serves (J9 slice)"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 4 "http://127.0.0.1:${PPROF_PORT}/debug/pprof/" 2>/dev/null)"
[[ "$code" == "200" ]] && pass "pprof index serves 200 on :$PPROF_PORT" || fail "pprof did not serve (code=$code)"
hcode="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 "http://127.0.0.1:${PPROF_PORT}/debug/pprof/heap" 2>/dev/null)"
[[ "$hcode" == "200" ]] && pass "pprof heap profile serves" || fail "pprof heap did not serve (code=$hcode)"

echo "== 05 OTLP export wired (metrics + logs)"
# No collector runs, so we assert the pipelines were BUILT (the exporters are
# lazy — a missing collector doesn't fail startup). Full arrival is validated
# against a real collector + the internal/telemetry bridge/pump unit tests.
if grep -qa '"msg":"otlp metrics export enabled"' "$DAEMON_LOG"; then
  pass "OTLP metric export enabled (bridges the Prometheus registry)"
else
  fail "OTLP metric export not enabled — expected the enable log line"
fi
if grep -qa '"msg":"otlp logs export enabled"' "$DAEMON_LOG"; then
  pass "OTLP log export enabled (taps the durable log-store fanout)"
else
  fail "OTLP log export not enabled — expected the enable log line"
fi

echo "== 06 packet capture (v0.5.4): host-side pcap of the app instance"
INST="$(curl -s "$BASE_URL/apps/web" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
if [[ "$INST" != sbx_* ]]; then
  fail "could not read web's instance id for the capture test"
elif ! command -v tcpdump >/dev/null 2>&1; then
  skip "tcpdump not installed on host — capture needs it"
else
  PCAP="$SMOKE_ROOT/web.pcap"
  # drive traffic through the proxy while capturing for a few seconds
  ( for _ in $(seq 1 60); do proxy_hit "web.${DOMAIN}" >/dev/null 2>&1; sleep 0.05; done ) &
  LOADPID=$!
  cli sandbox capture "$INST" -w "$PCAP" --max-seconds 4 --filter "tcp" >/dev/null 2>&1
  kill "$LOADPID" 2>/dev/null || true; wait "$LOADPID" 2>/dev/null || true
  if [[ -s "$PCAP" ]]; then
    MAGIC="$(head -c4 "$PCAP" | od -An -tx1 | tr -d ' \n')"
    # pcap magic: d4c3b2a1 (LE) / a1b2c3d4 (BE); pcapng: 0a0d0d0a
    if [[ "$MAGIC" == d4c3b2a1 || "$MAGIC" == a1b2c3d4 || "$MAGIC" == 0a0d0d0a ]]; then
      pass "captured a valid pcap ($(wc -c <"$PCAP") bytes, magic $MAGIC)"
    else
      fail "capture output is not a pcap (magic=$MAGIC)"
    fi
  else
    fail "capture produced no output"
  fi
  # audit line present in the daemon log
  grep -qa '"msg":"packet capture started"' "$DAEMON_LOG" \
    && pass "capture emitted an audit log line" \
    || fail "no packet-capture audit log line"
fi

echo "==============================================================="
echo " observability smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
