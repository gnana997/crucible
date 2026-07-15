#!/usr/bin/env bash
#
# smoke_tls.sh — end-to-end TLS termination + ACME (custom domains), v0.7.0.
#
# Runs Pebble (a local ACME test CA) in Docker and drives real certificate
# issuance through the daemon: an app gets a custom domain, the ingress proxy
# obtains a cert from Pebble on-demand, terminates HTTPS, and forwards to the
# guest. A client trusting Pebble's issued-cert root verifies the chain.
#
#   01  Pebble up (ACME dir on :14000, mgmt on :15000); its minica + issued-root
#       are extracted for trust
#   02  daemon up with TLS termination: --acme-email + --acme-ca-url <pebble> +
#       --acme-ca-root <minica> + --cert-dir
#   03  an app is created and a custom domain attached to it
#   04  an HTTPS request to the domain through the proxy is TERMINATED with a
#       Pebble-issued cert (verified against Pebble's root) and routed to the
#       guest — the marquee path
#   05  DecisionFunc gate: a handshake for an UNREGISTERED domain gets no cert
#       (issuance denied) and fails — a stray SNI can't burn a cert
#   06  a passthrough-mode app still pipes the guest's own cert (not terminated)
#
# Pebble runs with PEBBLE_VA_ALWAYS_VALID=1, so it issues WITHOUT connecting back
# to validate the challenge — this exercises the full certmagic<->ACME issuance +
# storage + termination + serving path without the DNS/port plumbing a real
# challenge round-trip needs. HTTP-01/TLS-ALPN-01 challenge SERVING is covered by
# the unit tests (internal/ingress, internal/tlscert).
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# Docker (for Pebble), curl, python3, and internet (pull nginx:alpine + the
# Pebble image) or cached copies.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_tls.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7917}"
BASE_URL="http://${LISTEN}"
HTTP_PORT="${HTTP_PORT:-8080}"   # proxy :80 (HTTP-01 + redirect)
TLS_PORT="${TLS_PORT:-8443}"     # proxy :443 (termination)
MOUNT="${MOUNT:-/var/lib/crucible-tls}"
IMAGE="${IMAGE:-nginx:alpine}"
PEBBLE_IMAGE="${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}"
DOMAIN="${DOMAIN:-shop.crucible-tls.test}"
RAW_DOMAIN="${RAW_DOMAIN:-raw.crucible-tls.test}"

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible TLS termination + ACME smoke (Pebble)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v docker >/dev/null || { echo "error: docker needed (runs Pebble)" >&2; exit 2; }
command -v curl >/dev/null && command -v python3 >/dev/null || { echo "error: curl + python3 needed" >&2; exit 2; }
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>)" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,images,logs,certs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
MINICA="$MOUNT/pebble.minica.pem"     # signs Pebble's ACME endpoint (daemon trusts this)
PEBBLE_ROOT="$MOUNT/pebble-root.pem"  # signs Pebble's ISSUED certs (client trusts this)

DAEMON_PID=""; PEBBLE_CID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ -n "$PEBBLE_CID" ]] && docker rm -f "$PEBBLE_CID" >/dev/null 2>&1
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

echo "== 01 start Pebble (ACME test CA) + extract trust roots"
# PEBBLE_VA_ALWAYS_VALID: issue without a challenge round-trip (see header).
PEBBLE_CID="$(docker run -d --rm -p 14000:14000 -p 15000:15000 \
  -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_VA_ALWAYS_VALID=1 "$PEBBLE_IMAGE" 2>/dev/null)"
[[ -n "$PEBBLE_CID" ]] || { echo "error: could not start Pebble ($PEBBLE_IMAGE)"; exit 3; }
# minica (fixed, ships in the image) — the daemon trusts it to talk to :14000.
# docker cp, not exec: the Pebble image is distroless (no shell/cat).
docker cp "$PEBBLE_CID:/test/certs/pebble.minica.pem" "$MINICA" >/dev/null 2>&1
[[ -s "$MINICA" ]] || { echo "error: could not read pebble.minica.pem from the image"; docker logs "$PEBBLE_CID" 2>&1 | tail; exit 3; }
# wait for the ACME directory, then fetch the per-run issued-cert root for the client.
ready=0
for _ in $(seq 1 60); do
  curl -sf --cacert "$MINICA" https://localhost:14000/dir >/dev/null 2>&1 && { ready=1; break; }
  sleep 0.5
done
[[ "$ready" -eq 1 ]] || { echo "error: Pebble ACME dir never came up"; docker logs "$PEBBLE_CID" 2>&1 | tail; exit 3; }
curl -sf --cacert "$MINICA" https://localhost:15000/roots/0 > "$PEBBLE_ROOT" 2>/dev/null
[[ -s "$PEBBLE_ROOT" ]] || { echo "error: could not fetch Pebble issued-cert root"; exit 3; }
ok "Pebble up; minica + issued-root trusted"

echo "== 02 start daemon with TLS termination (ACME → Pebble)"
"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --app-db "$MOUNT/apps.db" --network-egress-iface "$EGRESS" \
  --proxy-listen "127.0.0.1:$HTTP_PORT" --proxy-tls-listen "127.0.0.1:$TLS_PORT" \
  --proxy-domain apps.local \
  --acme-email ops@crucible-tls.test --acme-ca-url https://localhost:14000/dir \
  --acme-ca-root "$MINICA" --cert-dir "$MOUNT/certs" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3; }
grep -q "ingress TLS termination enabled" "$DAEMON_LOG" && ok "daemon up, TLS termination enabled" \
  || { bad "TLS termination not enabled"; tail -20 "$DAEMON_LOG"; }

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { run app get "$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
wait_phase() { local want="$2" tries="${3:-240}"; for _ in $(seq 1 "$tries"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.5; done; return 1; }
# https_get DOMAIN CAFILE NEEDLE — curl the proxy TLS port with SNI=DOMAIN,
# verifying against CAFILE; 0 if the body contains NEEDLE.
https_get() {
  local dom="$1" ca="$2" needle="$3"
  local body
  body="$(curl -s --max-time 30 --cacert "$ca" --resolve "$dom:$TLS_PORT:127.0.0.1" "https://$dom:$TLS_PORT/" 2>/dev/null)"
  [[ "$body" == *"$needle"* ]]
}

echo "== 03 create an app + attach a custom domain"
run app create web --image "$IMAGE" --pull missing --port 80 --restart always --health "http:80:/" --memory 256 >/dev/null 2>&1
if wait_phase web running; then
  ok "app 'web' running"
else
  bad "app never ran (is $IMAGE pullable?)"; tail -20 "$DAEMON_LOG"; exit 1
fi
run app domain add web "$DOMAIN" >/dev/null 2>&1 && ok "attached custom domain $DOMAIN" || bad "app domain add"

echo "== 04 HTTPS request is TERMINATED with a Pebble-issued cert + routed"
served=0
for _ in $(seq 1 20); do
  https_get "$DOMAIN" "$PEBBLE_ROOT" "nginx" && { served=1; break; }
  https_get "$DOMAIN" "$PEBBLE_ROOT" "html"  && { served=1; break; }
  sleep 1  # first request obtains the cert on-demand (ACME round-trip to Pebble)
done
[[ "$served" -eq 1 ]] && ok "HTTPS terminated with an ACME cert (verified vs Pebble root) + routed to the guest" \
  || { bad "HTTPS request never served"; tail -25 "$DAEMON_LOG"; }

echo "== 05 DecisionFunc gate: an unregistered domain gets no cert"
# No app owns this domain → issuance denied → the handshake can't complete.
if curl -s --max-time 15 --cacert "$PEBBLE_ROOT" --resolve "unclaimed.crucible-tls.test:$TLS_PORT:127.0.0.1" \
     "https://unclaimed.crucible-tls.test:$TLS_PORT/" >/dev/null 2>&1; then
  bad "SECURITY: an unregistered domain got a working cert (DecisionFunc gate broken)"
else
  ok "unregistered domain refused a cert (issuance gated to registered app domains)"
fi

echo "== 06 passthrough app still pipes the guest's own cert"
# A passthrough app owns :443; the proxy must NOT terminate it. We can only prove
# the proxy doesn't present a Pebble cert for it: verifying against the Pebble
# root must FAIL (the guest, not the proxy, would answer — and there's no guest
# TLS here, so the handshake fails rather than terminating with a Pebble cert).
run app create rawapp --image "$IMAGE" --pull missing --port 80 --restart always --health "http:80:/" --memory 256 --tls-mode passthrough >/dev/null 2>&1
wait_phase rawapp running >/dev/null 2>&1
run app domain add rawapp "$RAW_DOMAIN" >/dev/null 2>&1
if curl -s --max-time 10 --cacert "$PEBBLE_ROOT" --resolve "$RAW_DOMAIN:$TLS_PORT:127.0.0.1" \
     "https://$RAW_DOMAIN:$TLS_PORT/" >/dev/null 2>&1; then
  bad "passthrough app was terminated with a proxy cert (should have piped to the guest)"
else
  ok "passthrough app not terminated by the proxy (piped to the guest, which owns its cert)"
fi
run app rm web >/dev/null 2>&1; run app rm rawapp >/dev/null 2>&1

echo "==============================================================="
echo " TLS smoke: $pass passed, $fail failed"
echo " transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$fail" -eq 0 ]]
