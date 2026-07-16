#!/usr/bin/env bash
#
# smoke_key_rotation.sh — encryption key management (v0.8.1).
#
# Proves the keyring + rotation contract on a LIVE, running app:
#
#   03  a volume is created encrypted under the default key; an app boots on it
#       and a marker is written; `volume ls` shows KEY=default; on-disk = LUKS.
#   04  `volume rewrap --to-key extra` re-keys the volume WHILE THE APP RUNS —
#       no data is re-encrypted — and the app still reads its marker (KEY=extra).
#   05  reload REFUSES to drop a key a volume still uses (the safety guard).
#   06  rewrap back to default, then retire the now-unused `extra` key with
#       `volume keys reload` — succeeds, data intact.
#   07  the daemon logged volume_key_rotated audit records, and NO key material
#       (the base64 keys) appears anywhere in the log.
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# cryptsetup, curl, a pullable nginx:alpine, a default egress iface.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_key_rotation.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7928}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-keyrot}"
IMAGE="${IMAGE:-nginx:alpine}"
MARKER="ROTATE-marker-5b2e9a1c"

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible encryption key-rotation smoke (v0.8.1)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer + cryptsetup)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v cryptsetup >/dev/null || { echo "error: cryptsetup needed" >&2; exit 2; }
command -v curl       >/dev/null || { echo "error: curl needed" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>)" >&2; exit 2; }

echo "== 01 prepare work root + keyring (default.key + extra.key)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs,keys}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
KEYS="$MOUNT/keys"
DEFAULT_B64="$(head -c 32 /dev/urandom | base64)"
EXTRA_B64="$(head -c 32 /dev/urandom | base64)"
printf '%s' "$DEFAULT_B64" > "$KEYS/default.key"
printf '%s' "$EXTRA_B64"   > "$KEYS/extra.key"
chmod 600 "$KEYS"/*.key

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  for m in /dev/mapper/crucible-vol-* /dev/mapper/crucible-fmt-*; do
    [[ -e "$m" ]] && cryptsetup close "$(basename "$m")" 2>/dev/null || true
  done
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --volume-dir "$MOUNT/volumes" --volume-key-dir "$KEYS" --volume-default-key default \
  --volume-encrypt --network-egress-iface "$EGRESS" \
  --app-db "$MOUNT/apps.db" --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
grep -q "volume encryption enabled" "$DAEMON_LOG" && ok "daemon up, 2-key keyring loaded" || bad "encryption not enabled"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_running() { for _ in $(seq 1 400); do [[ "$(app_phase "$1")" == "running" ]] && return 0; sleep 0.3; done; return 1; }
vol_key() { run volume ls 2>/dev/null | awk -v n="$1" '$1==n {print $4}'; }
read_marker() { run app exec web -- cat /data/m 2>/dev/null | tr -d '[:space:]'; }

echo "== 02 create an encrypted volume (default key) + boot an app on it"
run volume create data --encrypt --size 256M >/dev/null 2>&1 && ok "volume create --encrypt" || bad "create failed"
[[ "$(vol_key data)" == "default" ]] && ok "'volume ls' shows KEY=default" || bad "KEY != default ($(vol_key data))"
head -c 6 "$MOUNT/volumes/data.img" | grep -aq LUKS && ok "on-disk container is LUKS" || bad "not a LUKS container"
cerr="$(run app create web --image "$IMAGE" --volume data:/data -p 18090:80 --restart always --vcpus 1 --memory 256 2>&1)" \
  || { bad "app create failed: $cerr"; tail -20 "$DAEMON_LOG"; exit 1; }
wait_running web && ok "app running on the encrypted volume" || { bad "app never ran"; run app get web 2>&1 | head -c 300; tail -20 "$DAEMON_LOG"; exit 1; }
run app exec web -- sh -c "echo $MARKER > /data/m && sync" >/dev/null 2>&1
[[ "$(read_marker)" == "$MARKER" ]] && ok "wrote + read marker in the guest" || bad "marker write/read failed"

echo "== 03 rewrap to 'extra' WHILE the app runs (live, no data re-encrypted)"
run volume rewrap data --to-key extra >/dev/null 2>&1 && ok "volume rewrap --to-key extra" || bad "rewrap failed"
[[ "$(vol_key data)" == "extra" ]] && ok "'volume ls' now shows KEY=extra" || bad "KEY != extra ($(vol_key data))"
[[ "$(read_marker)" == "$MARKER" ]] && ok "app still reads its data after live rotation" || bad "data lost after rewrap"

echo "== 04 reload REFUSES to drop a key a volume still uses"
mv "$KEYS/extra.key" "$KEYS/extra.key.bak"
if run volume keys reload >/dev/null 2>&1; then bad "reload dropped an in-use key (guard broken)"; else ok "reload refused (extra still in use)"; fi
mv "$KEYS/extra.key.bak" "$KEYS/extra.key"   # restore; the daemon kept the old ring
[[ "$(read_marker)" == "$MARKER" ]] && ok "volume still opens after the refused reload" || bad "data unreadable after refused reload"

echo "== 05 rewrap back to default, then retire 'extra' via reload"
run volume rewrap data --to-key default >/dev/null 2>&1 && ok "rewrap back to default" || bad "rewrap back failed"
[[ "$(vol_key data)" == "default" ]] && ok "KEY=default again" || bad "KEY != default"
rm -f "$KEYS/extra.key"
run volume keys reload >/dev/null 2>&1 && ok "keys reload retired 'extra' (now unused)" || bad "reload failed after retiring an unused key"
[[ "$(read_marker)" == "$MARKER" ]] && ok "data intact after retiring the old key" || bad "data lost after retire"

echo "== 06 audit records present, NO key material leaked to the log"
grep -q '"volume_key_rotated"' "$DAEMON_LOG" && ok "volume_key_rotated audit records logged" || bad "no rotation audit records"
if grep -aqF "$DEFAULT_B64" "$DAEMON_LOG" || grep -aqF "$EXTRA_B64" "$DAEMON_LOG"; then
  bad "SECURITY: key material appears in the daemon log"
else
  ok "no key material (base64 keys) anywhere in the log"
fi

run app rm web >/dev/null 2>&1 || true

echo "==============================================================="
echo " key-rotation smoke: $pass passed, $fail failed"
echo " transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$fail" -eq 0 ]]
