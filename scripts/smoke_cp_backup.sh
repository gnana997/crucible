#!/usr/bin/env bash
#
# Control-plane backup/restore acceptance (`crucible admin backup`).
#
# Proves the disaster story for the daemon's own state: every control-plane
# store is captured in one archive, and restoring it onto a wiped daemon brings
# back apps (which SELF-HEAL from the restored desired state), tokens, volume
# records, and registry credentials. Volume DATA is volume backup's job — this
# is the metadata half.
#
#   01  daemon starts with auth; an admin token + a read-scoped token are minted
#   02  state is created: a durable app, a volume, a registry credential
#   03  the read-scoped token gets 403 on /admin/backup (default-deny op);
#       the admin token downloads the archive; manifest + entries verified
#   04  the daemon is stopped and every control-plane store file is DELETED
#       (the volume backing file survives — data is not CP state)
#   05  the archive is restored file-by-file and the daemon restarts:
#       - the old admin token still authenticates (token store restored)
#       - the app record is back AND the reconciler re-creates a running
#         instance from it (restore = the app heals itself)
#       - the volume record is back, pointing at the surviving backing file
#       - the registry credential is back
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl,
# python3, tar, and internet (pulls nginx:alpine) or a cached image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_cp_backup.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7896}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-cpbackup-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; VOL_DIR="$SMOKE_ROOT/volumes"
APP_DB="$SMOKE_ROOT/apps.db"; TOKEN_FILE="$SMOKE_ROOT/tokens.json"
REG_STORE="$SMOKE_ROOT/registry.json"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR" "$VOL_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible control-plane backup/restore smoke"
echo " output dir : $SMOKE_ROOT"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null    || { echo "error: curl needed" >&2; exit 2; }
command -v python3 >/dev/null || { echo "error: python3 needed" >&2; exit 2; }
command -v tar >/dev/null     || { echo "error: tar needed" >&2; exit 2; }
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }
if systemctl is-active --quiet crucible 2>/dev/null; then
  echo "error: systemd crucible is active — stop it first (this starts its own daemon)" >&2; exit 2
fi

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --volume-dir "$VOL_DIR" \
    --token-file "$TOKEN_FILE" --registry-store "$REG_STORE" \
    --network-egress-iface "$EGRESS_IFACE" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}
stop_daemon() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  DAEMON_PID=""
}
cleanup() {
  if [[ -n "$DAEMON_PID" ]]; then
    "$CRUCIBLE_BIN" --addr "$LISTEN" --token "${ADMIN_KEY:-}" app rm web >/dev/null 2>&1 || true
    sleep 1
    stop_daemon
  fi
}
trap cleanup EXIT

echo "== 01 start daemon (auth on) + mint admin and read-scoped tokens"
start_daemon
ADMIN_KEY="$("$CRUCIBLE_BIN" daemon token add --token-file "$TOKEN_FILE" --name admin | grep -o 'crucible_[A-Za-z0-9_-]*')"
cat > "$SMOKE_ROOT/readonly.json" <<'EOF'
{"operations":["read"]}
EOF
READ_KEY="$("$CRUCIBLE_BIN" daemon token add --token-file "$TOKEN_FILE" --name readonly --policy "$SMOKE_ROOT/readonly.json" | grep -o 'crucible_[A-Za-z0-9_-]*')"
if [[ "$ADMIN_KEY" == crucible_* && "$READ_KEY" == crucible_* ]]; then
  pass "minted admin + read-scoped tokens"
else
  fail "token minting failed"; exit 1
fi
cli() { "$CRUCIBLE_BIN" --addr "$LISTEN" --token "$ADMIN_KEY" "$@"; }
phase() { cli app get web 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
wait_phase() { for _ in {1..240}; do [[ "$(phase)" == "$1" ]] && return 0; sleep 0.5; done; return 1; }

echo "== 02 create control-plane state: app + volume + registry credential"
if [[ "$(cli app create web --image "$IMAGE" --pull missing --restart always --memory 256 2>/dev/null)" == "web" ]] && wait_phase running; then
  pass "durable app running"
else
  fail "app create failed"; tail -20 "$DAEMON_LOG"; exit 1
fi
cli volume create data1 --size 64M >/dev/null 2>&1 \
  && pass "volume data1 created" || fail "volume create failed"
cli registry login ghcr.io --username smokeuser --password smokepass >/dev/null 2>&1 \
  && pass "registry credential stored" || fail "registry login failed"

echo "== 03 backup: scoped token denied; admin token downloads a sane archive"
BK="$SMOKE_ROOT/backup.tar.gz"
if "$CRUCIBLE_BIN" --addr "$LISTEN" --token "$READ_KEY" admin backup -w "$BK.denied" >/dev/null 2>&1; then
  fail "read-scoped token was allowed to download the control-plane backup!"
else
  pass "read-scoped token → denied (admin_backup is default-deny)"
fi
if cli admin backup -w "$BK" >/dev/null 2>&1 && [[ -s "$BK" ]]; then
  pass "admin token downloaded the archive ($(stat -c%s "$BK") bytes)"
else
  fail "admin backup failed"; exit 1
fi
LISTING="$(tar -tzf "$BK" 2>/dev/null)"
WANT_OK=1
for f in app.db tokens.json volume-index.db registry-credentials.json manifest.json; do
  grep -qx "$f" <<<"$LISTING" || { WANT_OK=0; fail "archive missing $f (has: $(tr '\n' ' ' <<<"$LISTING"))"; }
done
[[ "$WANT_OK" -eq 1 ]] && pass "archive holds all five entries"

echo "== 04 disaster: stop the daemon and DELETE every control-plane store"
cli app rm web >/dev/null 2>&1; sleep 2   # free the VM; CP records are what we test
stop_daemon
rm -f "$APP_DB" "$TOKEN_FILE" "$REG_STORE" "$VOL_DIR/index.db"
[[ ! -e "$APP_DB" && ! -e "$TOKEN_FILE" && -e "$VOL_DIR/data1.img" ]] \
  && pass "CP stores wiped; volume backing file survives (data ≠ CP state)" \
  || fail "wipe went wrong"

echo "== 05 restore the archive + restart: state and self-healing return"
RESTORE="$SMOKE_ROOT/restore"; mkdir -p "$RESTORE"
tar -xzf "$BK" -C "$RESTORE"
# The archive was taken BEFORE app rm, so the restored app store still holds
# 'web' as desired-running — the reconciler must resurrect it.
cp "$RESTORE/app.db" "$APP_DB"
cp "$RESTORE/tokens.json" "$TOKEN_FILE"
cp "$RESTORE/volume-index.db" "$VOL_DIR/index.db"
cp "$RESTORE/registry-credentials.json" "$REG_STORE"
start_daemon
if curl -sf -H "Authorization: Bearer $ADMIN_KEY" "$BASE_URL/whoami" >/dev/null 2>&1; then
  pass "pre-disaster admin token authenticates against the restored store"
else
  fail "restored token store rejects the old admin token"
fi
if wait_phase running; then
  pass "restored app record self-healed into a running instance"
else
  fail "app did not come back from the restored store: phase='$(phase)'"
  tail -20 "$DAEMON_LOG"
fi
cli volume ls 2>/dev/null | grep -q data1 \
  && pass "volume record restored (data1 listed)" || fail "volume record missing after restore"
cli registry ls 2>/dev/null | grep -q ghcr.io \
  && pass "registry credential restored" || fail "registry credential missing after restore"

cli app rm web >/dev/null 2>&1 || true; sleep 1
cli volume rm data1 >/dev/null 2>&1 || true

echo "==============================================================="
echo " control-plane backup smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
