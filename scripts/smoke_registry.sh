#!/usr/bin/env bash
#
# Private-registry credentials smoke (v0.4.4).
#
#   01  daemon healthy with the credential store enabled (--registry-store)
#   02  registry login/ls/logout round-trip — ls shows host+username but NEVER
#       the secret; logout removes it
#   03  a PUBLIC image still pulls + boots with the store enabled (the keychain
#       falls back to anonymous for registries it has no credential for)
#   04  (opt-in) a PRIVATE image fails to pull without a credential and succeeds
#       after `registry login` — set REGISTRY_HOST/REGISTRY_USER/REGISTRY_PASS
#       and PRIVATE_IMAGE (a real private image on a TLS registry, e.g. ghcr.io)
#
# The credential store holds usable secrets, so 02 also asserts the secret is
# never echoed back by `registry ls` (host + username only).
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built with an
# embedded agent (make build), curl, network to pull the images.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux scripts/smoke_registry.sh
#   # opt-in private pull:
#   sudo … REGISTRY_HOST=ghcr.io REGISTRY_USER=me REGISTRY_PASS=$TOKEN \
#          PRIVATE_IMAGE=ghcr.io/me/private:latest scripts/smoke_registry.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7896}"
BASE_URL="http://${LISTEN}"
PUB_IMAGE="${PUB_IMAGE:-nginx:alpine}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-reg-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
REG_STORE="$SMOKE_ROOT/registry.json"; DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible private-registry credentials smoke (v0.4.4)"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " store      : $REG_STORE"
echo "==============================================================="

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }; done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]] || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }

if ! LC_ALL=C grep -qa "registry-store\|registry login" "$CRUCIBLE_BIN"; then
  echo "error: $CRUCIBLE_BIN predates private-registry credentials (v0.4.4). Rebuild: make build" >&2
  exit 2
fi

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0; SKIP=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
skip() { SKIP=$((SKIP+1)); echo "   SKIP: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --app-db "$WORK_BASE-apps.db" --image-dir "$IMAGE_DIR" \
    --registry-store "$REG_STORE" --network-egress-iface "$EGRESS_IFACE" \
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
  for id in $(cli sandbox ls -o json 2>/dev/null | grep -o 'sbx_[a-z0-9]*' | sort -u); do
    cli sandbox rm "$id" >/dev/null 2>&1 || true
  done
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null
  [[ -n "$DAEMON_PID" ]] && wait "$DAEMON_PID" 2>/dev/null
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$SMOKE_ROOT"
}
trap cleanup EXIT

echo "== 01 starting daemon (registry store enabled)"
start_daemon
grep -qa '"registry_store"\|registry-store\|healthz' "$DAEMON_LOG" >/dev/null 2>&1
pass "daemon healthy with --registry-store $REG_STORE"

# ---- 02 login / ls / logout round-trip; the secret is never echoed ----------
echo "== 02 registry login/ls/logout — the secret is never shown by ls"
DUMMY_HOST="smoke-registry.invalid"
DUMMY_USER="smoke-user"
DUMMY_SECRET="SMOKE-SECRET-DO-NOT-LEAK"
if printf '%s' "$DUMMY_SECRET" | cli registry login "$DUMMY_HOST" -u "$DUMMY_USER" --password-stdin >/dev/null 2>&1; then
  LS="$(cli registry ls 2>/dev/null)"
  LSJSON="$(cli registry ls -o json 2>/dev/null)"
  if [[ "$LS" == *"$DUMMY_HOST"* && "$LS" == *"$DUMMY_USER"* ]]; then
    pass "registry ls shows the logged-in host + username"
  else
    fail "registry ls missing the host/username: $LS"
  fi
  if [[ "$LS" == *"$DUMMY_SECRET"* || "$LSJSON" == *"$DUMMY_SECRET"* ]]; then
    fail "registry ls LEAKED the secret!"
  else
    pass "registry ls never exposes the secret (table + json)"
  fi
  # The on-disk store is 0600 and does hold the secret (usable creds).
  MODE="$(stat -c '%a' "$REG_STORE" 2>/dev/null)"
  [[ "$MODE" == "600" ]] && pass "credential store file mode is 600" || fail "store mode = ${MODE:-?}, want 600"
  cli registry logout "$DUMMY_HOST" >/dev/null 2>&1
  if cli registry ls 2>/dev/null | grep -q "$DUMMY_HOST"; then
    fail "logout did not remove the credential"
  else
    pass "registry logout removed the credential"
  fi
else
  fail "registry login failed (pre-v0.4.4 daemon?)"
fi

# ---- 03 public image still pulls with the store enabled (anonymous fallback) -
echo "== 03 public image ($PUB_IMAGE) still pulls with the store enabled"
SBX="$(cli run "$PUB_IMAGE" --memory 256 2>/dev/null)"
if [[ "$SBX" == sbx_* ]]; then
  pass "public pull works with --registry-store set (keychain falls back to anonymous)"
  cli sandbox rm "$SBX" >/dev/null 2>&1
else
  fail "public pull failed with the store enabled (anonymous fallback broken?)"; tail -20 "$DAEMON_LOG"
fi

# ---- 04 (opt-in) private image: fails without a cred, succeeds after login ---
echo "== 04 private image: fails without a credential, succeeds after login (opt-in)"
if [[ -z "${PRIVATE_IMAGE:-}" || -z "${REGISTRY_HOST:-}" || -z "${REGISTRY_USER:-}" || -z "${REGISTRY_PASS:-}" ]]; then
  skip "private-pull test off — set REGISTRY_HOST/REGISTRY_USER/REGISTRY_PASS + PRIVATE_IMAGE (a real private image)"
else
  # Ensure no credential yet: a pull must fail with an auth error.
  cli registry logout "$REGISTRY_HOST" >/dev/null 2>&1 || true
  if cli run "$PRIVATE_IMAGE" --pull always --memory 256 >/dev/null 2>&1; then
    fail "private image pulled WITHOUT a credential — auth not enforced!"
  else
    pass "private image refused without a credential"
  fi
  # Log in, then the pull must succeed.
  if printf '%s' "$REGISTRY_PASS" | cli registry login "$REGISTRY_HOST" -u "$REGISTRY_USER" --password-stdin >/dev/null 2>&1; then
    SBXP="$(cli run "$PRIVATE_IMAGE" --pull always --memory 256 2>/dev/null)"
    if [[ "$SBXP" == sbx_* ]]; then
      pass "private image pulled + booted after registry login ($SBXP)"
      cli sandbox rm "$SBXP" >/dev/null 2>&1
    else
      fail "private image still failed after login"; tail -20 "$DAEMON_LOG"
    fi
    cli registry logout "$REGISTRY_HOST" >/dev/null 2>&1
  else
    fail "registry login for $REGISTRY_HOST failed"
  fi
fi

echo "==============================================================="
echo " registry smoke: $PASS passed, $FAIL failed, $SKIP skipped"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
