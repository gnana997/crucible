#!/usr/bin/env bash
#
# smoke_incremental_backup.sh — end-to-end check for incremental backups (v0.9.3).
#
# Boots a real daemon under jailer with a --volume-dir, then verifies:
#   1. A full backup, then an INCREMENTAL against it, ships only the changed
#      blocks (the .delta is far smaller than the full .img).
#   2. Restoring the incremental TIP reassembles base + delta: both the base data
#      and the later-added data are present in the restored volume.
#   3. `backup ls` shows KIND / PARENT; deleting the parent is refused while the
#      incremental depends on it.
#   4. The same for an ENCRYPTED volume (ciphertext delta).
#   5. Export the chain (full + delta) off-host and import it back to fresh ids,
#      then restore the tip — data intact (the off-host chain round-trip).
#
# Requires: root + KVM, firecracker + jailer + vmlinux + a rootfs whose agent
# has /mount, mkfs.ext4, cryptsetup (for the encrypted case).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_incremental_backup.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7917}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-inctest}"
IMAGE="${IMAGE:-alpine:latest}"
VOLSZ=$((128*1024*1024))

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible incremental-backup smoke (v0.9.3)"
echo "==============================================================="

[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null || { echo "error: mkfs.ext4 needed (e2fsprogs)" >&2; exit 2; }
HAVE_CRYPT=1; command -v cryptsetup >/dev/null || HAVE_CRYPT=0
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

echo "== 01 prepare work root ($MOUNT)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
KEYFILE="$MOUNT/volkey"; head -c 32 /dev/urandom | base64 > "$KEYFILE"
BK="$MOUNT/volumes/backups"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" != "1" ]] && rm -rf "$MOUNT"
}
trap cleanup EXIT

echo "== 02 start daemon"
"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --volume-dir "$MOUNT/volumes" --volume-encrypt-key-file "$KEYFILE" \
  --app-db "$MOUNT/apps.db" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
echo "   daemon healthy (pid $DAEMON_PID)"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
fsize() { stat -c %s "$1" 2>/dev/null || echo 0; }

# write_marker <volume> <file> <content> — attach in a fresh sandbox, write, detach.
write_marker() {
  local sbx; sbx=$(run sandbox create --image "$IMAGE" --volume "$1":/vol 2>/dev/null)
  [[ -n "$sbx" ]] || { bad "attach $1 to write $2"; return 1; }
  run sandbox exec "$sbx" -- sh -c "echo $3 > /vol/$2 && sync" >/dev/null 2>&1
  run rm "$sbx" >/dev/null 2>&1 || true
}
# read_marker <volume> <file> — attach, cat, detach; echoes the content.
read_marker() {
  local sbx got; sbx=$(run sandbox create --image "$IMAGE" --volume "$1":/vol 2>/dev/null)
  [[ -n "$sbx" ]] || { echo ""; return 1; }
  got=$(run sandbox exec "$sbx" -- cat /vol/"$2" 2>/dev/null | tr -d '\r\n')
  run rm "$sbx" >/dev/null 2>&1 || true
  echo "$got"
}

# chain_case <vol> <restored> <create-flags...> — full -> incremental -> restore tip.
# Sets globals FULL_ID / INC_ID for the caller.
chain_case() {
  local vol="$1" restored="$2"; shift 2
  run volume create "$vol" --size "$VOLSZ" "$@" >/dev/null 2>&1 || { bad "create $vol"; return; }
  write_marker "$vol" m1 base || return
  FULL_ID=$(run volume backup "$vol" 2>/dev/null | tr -d '\r\n')
  [[ -n "$FULL_ID" ]] && ok "full backup $vol ($FULL_ID)" || { bad "full backup $vol"; return; }

  write_marker "$vol" m2 added || return
  INC_ID=$(run volume backup "$vol" --parent "$FULL_ID" 2>/dev/null | tr -d '\r\n')
  [[ -n "$INC_ID" ]] && ok "incremental backup $vol ($INC_ID)" || { bad "incremental $vol"; return; }

  local fullsz incsz
  fullsz=$(fsize "$BK/$vol/$FULL_ID.img")
  incsz=$(fsize "$BK/$vol/$INC_ID.delta")
  if [[ "$incsz" -gt 0 && "$fullsz" -gt 0 ]] && (( incsz * 4 < fullsz )); then
    ok "delta far smaller than full ($incsz vs $fullsz bytes)"
  else
    bad "delta not small enough ($incsz vs $fullsz bytes)"
  fi

  run volume restore --from "$INC_ID" --to "$restored" >/dev/null 2>&1 \
    && ok "restore incremental tip → $restored" || { bad "restore $restored"; return; }
  local g1 g2; g1=$(read_marker "$restored" m1); g2=$(read_marker "$restored" m2)
  [[ "$g1" == "base" ]] && ok "base data present after chain restore (m1=$g1)" || bad "m1='$g1' want 'base'"
  [[ "$g2" == "added" ]] && ok "incremental data present after chain restore (m2=$g2)" || bad "m2='$g2' want 'added'"
}

echo "== 03 plaintext: full → incremental → restore tip"
chain_case idata irestored
PFULL="$FULL_ID"; PINC="$INC_ID"

echo "== 04 backup ls shows kind/parent; deleting the parent is refused"
run volume backup ls idata 2>/dev/null | grep -q incremental \
  && ok "backup ls shows the incremental" || bad "backup ls missing kind"
run volume backup rm "$PFULL" >/dev/null 2>&1 \
  && bad "deleted a parent with a live child" || ok "parent delete refused (has dependent incremental)"

echo "== 05 encrypted: full → incremental → restore tip (ciphertext delta)"
if [[ "$HAVE_CRYPT" == 1 ]]; then
  chain_case edata erestored --encrypt
else
  echo "   (skipped: cryptsetup not installed)"
fi

echo "== 06 off-host chain round-trip: export full+delta, import to fresh ids, restore"
EXP="$MOUNT/exp"; mkdir -p "$EXP"
run volume backup export "$PFULL" -w "$EXP/full.img.gz" >/dev/null 2>&1 \
  && ok "exported the full" || bad "export full"
run volume backup export "$PINC" -w "$EXP/inc.delta.gz" >/dev/null 2>&1 \
  && ok "exported the delta" || bad "export delta"
F2=$(run volume backup import --source idata2 -f "$EXP/full.img.gz" 2>/dev/null | tr -d '\r\n')
[[ -n "$F2" ]] && ok "imported full → $F2" || bad "import full"
I2=$(run volume backup import --source idata2 --parent "$F2" -f "$EXP/inc.delta.gz" 2>/dev/null | tr -d '\r\n')
[[ -n "$I2" ]] && ok "imported delta (parent $F2) → $I2" || bad "import delta"
if [[ -n "${I2:-}" ]]; then
  run volume restore --from "$I2" --to idata2restored >/dev/null 2>&1 \
    && ok "restored the imported chain tip" || bad "restore imported chain"
  g1=$(read_marker idata2restored m1); g2=$(read_marker idata2restored m2)
  [[ "$g1" == "base" && "$g2" == "added" ]] \
    && ok "imported chain restore has base + incremental data" \
    || bad "imported chain data wrong (m1='$g1' m2='$g2')"
fi

echo "==============================================================="
echo " result: $pass passed, $fail failed"
echo "==============================================================="
[[ $fail -eq 0 ]] || exit 1
