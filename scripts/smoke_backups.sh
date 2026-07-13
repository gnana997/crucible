#!/usr/bin/env bash
#
# smoke_backups.sh — end-to-end check for volume backups (v0.6.3, B1).
#
# Boots a real daemon under jailer with a --volume-dir, then verifies:
#   1. A DETACHED volume backs up, and the backup is a real, mountable ext4
#      image with the data intact (loop-mounted read-only on the host).
#   2. `volume backup ls` lists it.
#   3. A SLEPT volume app (v0.6.2) backs up (its backing file is quiescent +
#      host-fsync'd), and that backup also loop-mounts with the data intact.
#   4. A LIVE (running) volume app REFUSES a backup (409) — no-downtime live
#      backup needs the fsfreeze agent op (a later milestone).
#   5. `volume backup rm` removes the backup file + record.
#
# Requires: root + KVM, firecracker + jailer + vmlinux + a rootfs whose agent
# has /mount, mkfs.ext4, curl, and host `mount -o loop` (root). A long-running
# image (nginx:alpine) must be pullable or pre-present.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_backups.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7913}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-bkptest}"
IMAGE="${IMAGE:-alpine:latest}"
APP_IMAGE="${APP_IMAGE:-nginx:alpine}"

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible volume-backups smoke (v0.6.3 B1)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer + loop mount)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null || { echo "error: mkfs.ext4 needed (e2fsprogs)" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

echo "== 01 prepare work root ($MOUNT)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"

DAEMON_PID=""
cleanup() {
  umount "$MOUNT/mp" 2>/dev/null || true
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

echo "== 02 start daemon (--volume-dir $MOUNT/volumes; backups default to <volume-dir>/backups)"
"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --volume-dir "$MOUNT/volumes" --volume-default-size $((256*1024*1024)) \
  --app-db "$MOUNT/apps.db" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
echo "   daemon healthy (pid $DAEMON_PID)"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_phase() { local want="$2" t="${3:-400}"; for _ in $(seq 1 "$t"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.05; done; return 1; }

# loop-mount a backup .img read-only and check /marker == expected. The backup's
# ext4 was never cleanly unmounted (the guest was destroyed / is snapshot-frozen),
# so its journal is in a recoverable-but-not-clean state; `ro,noload` skips journal
# replay so a read-only host mount succeeds (a real restore mounts it rw + replays).
verify_marker() { # <backup.img> <expected>
  local img="$1" want="$2" mp="$MOUNT/mp" got err
  mkdir -p "$mp"
  if ! err=$(mount -o loop,ro,noload "$img" "$mp" 2>&1); then
    echo "     (loop-mount failed for $img: $err)"; return 1
  fi
  got=$(cat "$mp/marker" 2>/dev/null | tr -d '\r\n')
  umount "$mp" 2>/dev/null || true
  [[ "$got" == "$want" ]] && return 0
  echo "     (marker='$got' want='$want')"; return 1
}
newest_img() { ls -t "$MOUNT"/volumes/backups/"$1"/*.img 2>/dev/null | head -1; }

# ---- 03 detached volume backs up; the backup is a mountable ext4 with data --
echo "== 03 back up a DETACHED volume; the backup is a real ext4 with the data"
SBX=$(run sandbox create --image "$IMAGE" --volume data:/vol 2>/dev/null)
if [[ -n "${SBX:-}" ]]; then
  run sandbox exec "$SBX" -- sh -c 'echo detached-ok > /vol/marker && sync' >/dev/null 2>&1 \
    && ok "wrote /vol/marker" || bad "write to volume"
  run rm "$SBX" >/dev/null 2>&1 || true   # volume now detached (no live writer)
  BID=$(run volume backup data 2>/dev/null | tr -d '\r\n')
  [[ -n "$BID" ]] && ok "volume backup data → $BID" || bad "backup returned no id"
  BIMG=$(newest_img data)
  if [[ -n "$BIMG" ]] && verify_marker "$BIMG" "detached-ok"; then
    ok "backup loop-mounts read-only with the data intact"
  else
    bad "backup did not contain the expected data"
  fi
else
  bad "create sandbox with volume"
fi

# ---- 04 backup ls lists it --------------------------------------------------
echo "== 04 backup appears in the listing"
run volume backup ls data 2>/dev/null | grep -q "${BID:-__none__}" \
  && ok "volume backup ls shows $BID" || bad "backup not listed"

# ---- 05 slept volume app backs up (quiescent) ------------------------------
echo "== 05 back up a SLEPT volume app (quiescent, host-fsync'd)"
run app rm bkpapp >/dev/null 2>&1 || true; run volume rm appdata >/dev/null 2>&1 || true
run app create bkpapp --image "$APP_IMAGE" --restart always \
  --volume appdata:/data >/dev/null 2>&1
if wait_phase bkpapp running 600; then
  run app exec bkpapp -- sh -c 'echo slept-ok > /data/marker && sync' >/dev/null 2>&1 \
    && ok "wrote /data/marker in the running app" || bad "app exec write"
  run app sleep bkpapp >/dev/null 2>&1
  if wait_phase bkpapp asleep 200; then
    ok "app slept (snapshot, VMM stopped)"
    SBID=$(run volume backup appdata 2>/dev/null | tr -d '\r\n')
    SIMG=$(newest_img appdata)
    if [[ -n "$SBID" && -n "$SIMG" ]] && verify_marker "$SIMG" "slept-ok"; then
      ok "slept-volume backup has the data intact ($SBID)"
    else
      bad "slept-volume backup missing/wrong data"
    fi
  else
    bad "app never reached asleep"
  fi
else
  bad "volume app never booted"
fi

# ---- 06 live (running) volume app REFUSES a backup -------------------------
echo "== 06 a LIVE volume app refuses a backup (needs fsfreeze, a later release)"
run app wake bkpapp >/dev/null 2>&1
if wait_phase bkpapp running 200; then
  if run volume backup appdata >/dev/null 2>&1; then
    bad "live backup was NOT refused (should 409 until fsfreeze lands)"
  else
    ok "live backup refused while the app is running"
  fi
else
  bad "app did not wake for the live-refusal check"
fi

# ---- 07 backup rm removes the file + record --------------------------------
echo "== 07 backup rm removes the backup file and record"
if [[ -n "${BID:-}" ]]; then
  run volume backup rm "$BID" >/dev/null 2>&1 && ok "volume backup rm $BID" || bad "backup rm"
  [[ -z "$(newest_img data)" ]] && ok "backup file gone after rm" || bad "backup file still present"
  run volume backup ls data 2>/dev/null | grep -q "$BID" && bad "backup still listed after rm" \
    || ok "backup no longer listed"
fi

run app rm bkpapp >/dev/null 2>&1 || true

echo "==============================================================="
echo " backups smoke: $pass passed, $fail failed"
echo "==============================================================="
[[ $fail -eq 0 ]]
