#!/usr/bin/env bash
#
# Off-host volume backup round trip (export → import → restore).
#
# Proves the disaster loop that keeps backups from dying with the box: a backup
# is streamed OFF the host (as the control plane would, to ship it to an object
# store), the volume and its local backup are then destroyed, the backup is
# streamed back ON, restored to a new volume, and the data is intact. Plus the
# security gate: streaming a backup off the host needs the default-deny
# `volume_backup` scoped op, not `read`.
#
#   01  work root + a daemon with --volume-dir and auth ON
#   02  mint an admin token (drives) + a read-only scoped token (the gate)
#   03  create a volume, write a marker into it via a sandbox, detach
#   04  `volume backup` (detached → consistent copy)
#   05  GATE: a read-only token is refused `volume backup export` (needs
#       volume_backup); the admin token exports it to a gzip file off the host
#   06  DISASTER: delete the local backup AND the volume (data gone locally)
#   07  `volume backup import` the file back → a fresh backup id, then
#       `volume restore` it to a NEW volume
#   08  attach the restored volume to a sandbox and read the marker → intact
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# curl, gzip, and internet (pulls alpine) or a cached image. Uses a /var/lib
# work root so volumes hardlink into the jail.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_offhost_backup.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7915}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-offhost}"
IMAGE="${IMAGE:-alpine:latest}"
MARKER="offhost-canary-$$-$RANDOM"

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible off-host backup round trip (export / import / restore)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null || { echo "error: mkfs.ext4 needed (e2fsprogs)" >&2; exit 2; }
command -v gzip >/dev/null || { echo "error: gzip needed" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

echo "== 01 prepare work root ($MOUNT)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
TOKEN_FILE="$MOUNT/tokens.json"; DAEMON_LOG="$MOUNT/daemon.log"
BK="$MOUNT/backup.img.gz"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

echo "== 02 start daemon (auth on, --volume-dir) + mint tokens"
"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --volume-dir "$MOUNT/volumes" --volume-default-size $((256*1024*1024)) \
  --app-db "$MOUNT/apps.db" --token-file "$TOKEN_FILE" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
ADMIN="$("$CRUCIBLE_BIN" daemon token add --token-file "$TOKEN_FILE" --name admin | grep -o 'crucible_[A-Za-z0-9_-]*')"
printf '{"operations":["read"]}\n' > "$MOUNT/ro.json"
READ="$("$CRUCIBLE_BIN" daemon token add --token-file "$TOKEN_FILE" --name ro --policy "$MOUNT/ro.json" | grep -o 'crucible_[A-Za-z0-9_-]*')"
[[ "$ADMIN" == crucible_* && "$READ" == crucible_* ]] && ok "daemon up + tokens minted" || { bad "token mint failed"; exit 1; }
run()  { "$CRUCIBLE_BIN" --addr "$LISTEN" --token "$ADMIN" "$@"; }
rocli(){ "$CRUCIBLE_BIN" --addr "$LISTEN" --token "$READ" "$@"; }

echo "== 03 write a marker into a volume via a sandbox, then detach"
# --volume auto-creates 'data' on first use (same as smoke_volumes.sh).
SBX="$(run sandbox create --image "$IMAGE" --pull missing --volume data:/vol 2>"$MOUNT/create.err")"
if [[ "$SBX" == sbx_* ]]; then
  ok "volume 'data' created + attached to a sandbox"
  run sandbox exec "$SBX" -- sh -c "echo $MARKER > /vol/marker && sync" >/dev/null 2>&1 \
    && ok "wrote marker into the volume" || bad "write marker"
  run sandbox rm "$SBX" >/dev/null 2>&1   # detach so the backup sees a quiescent volume
else
  bad "could not create sandbox to seed the volume: $(cat "$MOUNT/create.err" 2>/dev/null | head -c 300)"
  tail -20 "$DAEMON_LOG"; exit 1
fi

echo "== 04 back up the (detached) volume"
BID="$(run volume backup data 2>/dev/null | tr -d '[:space:]')"
[[ -n "$BID" ]] && ok "backup created: $BID" || { bad "volume backup"; tail -20 "$DAEMON_LOG"; exit 1; }

echo "== 05 export: read-only token DENIED; admin token streams it off-host"
if rocli volume backup export "$BID" -w "$MOUNT/denied.gz" >/dev/null 2>&1; then
  bad "read-only token was allowed to export a backup! (volume_backup gate broken)"
else
  ok "read-only token refused export (volume_backup is default-deny)"
fi
if run volume backup export "$BID" -w "$BK" >/dev/null 2>&1 && [[ -s "$BK" ]]; then
  # gzip magic 1f 8b confirms the default-compressed stream (od is coreutils).
  [[ "$(head -c2 "$BK" | od -An -tx1 | tr -d ' ')" == "1f8b" ]] \
    && ok "exported a gzip stream off-host ($(stat -c%s "$BK") bytes)" \
    || bad "export produced a non-gzip file"
else
  bad "admin export failed"; exit 1
fi

echo "== 06 DISASTER: delete the local backup AND the volume"
run volume backup rm "$BID" >/dev/null 2>&1 && ok "local backup deleted" || bad "backup rm"
run volume rm data >/dev/null 2>&1 && ok "volume deleted (data gone locally)" || bad "volume rm"
run volume ls 2>/dev/null | grep -q '\bdata\b' && bad "volume 'data' still present after rm" || ok "confirmed: volume 'data' is gone"

echo "== 07 import the shipped backup back, restore to a NEW volume"
NBID="$(run volume backup import --source data -f "$BK" 2>/dev/null | tr -d '[:space:]')"
[[ -n "$NBID" && "$NBID" != "$BID" ]] && ok "imported → fresh backup id: $NBID" || bad "import (got '$NBID')"
run volume restore --from "$NBID" --to restored >/dev/null 2>&1 \
  && ok "restored backup into new volume 'restored'" || { bad "restore"; tail -20 "$DAEMON_LOG"; exit 1; }

echo "== 08 attach the restored volume and read the marker → intact"
SBX2="$(run sandbox create --image "$IMAGE" --pull missing --volume restored:/vol 2>"$MOUNT/create2.err")"
if [[ "$SBX2" == sbx_* ]]; then
  GOT="$(run sandbox exec "$SBX2" -- cat /vol/marker 2>/dev/null | tr -d '\r\n')"
  [[ "$GOT" == "$MARKER" ]] && ok "marker survived export→import→restore ($GOT)" \
    || bad "marker lost/wrong after round trip: got '$GOT', want '$MARKER'"
  run sandbox rm "$SBX2" >/dev/null 2>&1
else
  bad "could not attach the restored volume: $(cat "$MOUNT/create2.err" 2>/dev/null | head -c 300)"
fi
run volume rm restored >/dev/null 2>&1 || true

echo "==============================================================="
echo " off-host backup smoke: $pass passed, $fail failed"
echo " transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$fail" -eq 0 ]]
