#!/usr/bin/env bash
#
# smoke_volume_grow.sh — end-to-end check for online volume grow (v0.9.1).
#
# Boots a real daemon under jailer with a --volume-dir, then verifies:
#   1. A DETACHED plaintext volume grows: the record size updates AND, once
#      re-attached, the guest's filesystem reports the larger capacity with the
#      pre-grow data still intact.
#   2. Shrinking is refused (grow-only), and growing an ATTACHED volume is
#      refused (409 — stop the app first; a snapshot pins the guest device size).
#   3. A DETACHED encrypted volume grows the same way (LUKS container + mapping
#      + ext4 all resized), data intact.
#
# Requires: root + KVM, firecracker + jailer + vmlinux + a rootfs whose agent
# has /mount, mkfs.ext4, resize2fs, cryptsetup (for the encrypted case).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_volume_grow.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7916}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-growtest}"
IMAGE="${IMAGE:-alpine:latest}"
APP_IMAGE="${APP_IMAGE:-nginx:alpine}"

# Sizes (bytes): grow 128 MiB -> 512 MiB; shrink attempt 64 MiB.
SZ_INIT=$((128*1024*1024))
SZ_GROWN=$((512*1024*1024))
SZ_SMALL=$((64*1024*1024))

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible volume-grow smoke (v0.9.1)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null || { echo "error: mkfs.ext4 needed (e2fsprogs)" >&2; exit 2; }
command -v resize2fs >/dev/null || { echo "error: resize2fs needed (e2fsprogs)" >&2; exit 2; }
command -v e2fsck >/dev/null || { echo "error: e2fsck needed (e2fsprogs)" >&2; exit 2; }
HAVE_CRYPT=1; command -v cryptsetup >/dev/null || HAVE_CRYPT=0
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

echo "== 01 prepare work root ($MOUNT)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
# A master key so encrypted volumes can be created + grown.
KEYFILE="$MOUNT/volkey"
head -c 32 /dev/urandom | base64 > "$KEYFILE"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" != "1" ]] && rm -rf "$MOUNT"
}
trap cleanup EXIT

echo "== 02 start daemon (--volume-dir; master key for encryption)"
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
vol_size() { curl -s "$BASE_URL/volumes/$1" 2>/dev/null | grep -o '"size_bytes":[0-9]*' | head -1 | grep -o '[0-9]*'; }
vol_attached() { curl -s "$BASE_URL/volumes/$1" 2>/dev/null | grep -o '"attached_to":"[^"]*"' | head -1; }
# guest_kb: 1K-blocks total of the filesystem mounted at path, inside sandbox $1.
guest_kb() { run sandbox exec "$1" -- df -Pk "$2" 2>/dev/null | awk 'NR==2{print $2}'; }
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_phase() { local want="$2" t="${3:-400}"; for _ in $(seq 1 "$t"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.05; done; return 1; }
app_kb() { run app exec "$1" -- df -Pk "$2" 2>/dev/null | awk 'NR==2{print $2}'; }

# grow_case <volname> <create-flags...> — the shared plaintext/encrypted flow.
grow_case() {
  local vol="$1"; shift
  run volume create "$vol" --size "$SZ_INIT" "$@" >/dev/null 2>&1 \
    && ok "created $vol ($((SZ_INIT/1024/1024)) MiB)" || { bad "create $vol"; return; }

  local sbx got0
  sbx=$(run sandbox create --image "$IMAGE" --volume "$vol":/vol 2>/dev/null)
  [[ -n "$sbx" ]] || { bad "attach $vol pre-grow"; return; }
  run sandbox exec "$sbx" -- sh -c 'echo grow-ok > /vol/marker && sync' >/dev/null 2>&1 \
    && ok "wrote marker to $vol" || bad "write marker to $vol"
  got0=$(guest_kb "$sbx" /vol)
  run rm "$sbx" >/dev/null 2>&1 || true   # detach

  local gerr
  if gerr=$(run volume grow "$vol" --size "$SZ_GROWN" 2>&1); then
    ok "volume grow $vol --size $((SZ_GROWN/1024/1024))M"
  else
    bad "grow $vol: $gerr"; return
  fi
  local rec; rec=$(vol_size "$vol")
  [[ "$rec" == "$SZ_GROWN" ]] && ok "record size updated to $rec" \
    || bad "record size = $rec, want $SZ_GROWN"

  local sbx2 got1 marker
  sbx2=$(run sandbox create --image "$IMAGE" --volume "$vol":/vol 2>/dev/null)
  [[ -n "$sbx2" ]] || { bad "re-attach $vol post-grow"; return; }
  marker=$(run sandbox exec "$sbx2" -- cat /vol/marker 2>/dev/null | tr -d '\r\n')
  [[ "$marker" == "grow-ok" ]] && ok "data intact after grow (marker=$marker)" \
    || bad "$vol marker='$marker' want 'grow-ok'"
  got1=$(guest_kb "$sbx2" /vol)
  if [[ -n "$got0" && -n "$got1" ]] && (( got1 > got0 * 3 )); then
    ok "guest filesystem grew (${got0}K -> ${got1}K blocks)"
  else
    bad "guest fs did not grow (${got0}K -> ${got1}K blocks)"
  fi
  run rm "$sbx2" >/dev/null 2>&1 || true
}

echo "== 03 grow a DETACHED plaintext volume; guest sees the new size"
grow_case data

echo "== 04 shrink is refused; growing an ATTACHED volume is refused"
run volume grow data --size "$SZ_SMALL" >/dev/null 2>&1 \
  && bad "shrink was allowed" || ok "shrink refused (grow-only)"
ASBX=$(run sandbox create --image "$IMAGE" --volume data:/vol 2>/dev/null)
if [[ -n "$ASBX" ]]; then
  run volume grow data --size $((768*1024*1024)) >/dev/null 2>&1 \
    && bad "grew an attached volume" || ok "grow refused while attached (409)"
  run rm "$ASBX" >/dev/null 2>&1 || true
else
  bad "attach for the attached-refusal check"
fi

echo "== 05 grow a DETACHED encrypted volume (LUKS + ext4 resized)"
if [[ "$HAVE_CRYPT" == 1 ]]; then
  grow_case enc --encrypt
else
  echo "   (skipped: cryptsetup not installed)"
fi

echo "== 06 the app recipe: app stop -> volume grow -> app start"
run volume create appdata --size "$SZ_INIT" >/dev/null 2>&1 && ok "created appdata" || bad "create appdata"
run app create growapp --image "$APP_IMAGE" --restart always --volume appdata:/data >/dev/null 2>&1
if wait_phase growapp running 600; then
  ok "volume app booted"
  run app exec growapp -- sh -c 'echo recipe-ok > /data/marker && sync' >/dev/null 2>&1 \
    && ok "wrote marker via the running app" || bad "app exec write"
  before=$(app_kb growapp /data)
  # while attached, grow is refused (detached-only).
  run volume grow appdata --size "$SZ_GROWN" >/dev/null 2>&1 \
    && bad "grew an attached app volume" || ok "grow refused while the app holds it (409)"
  # stop -> detaches the volume.
  run app stop growapp >/dev/null 2>&1 && ok "app stop returned" || bad "app stop"
  # attached_to is omitempty, so it is absent from the JSON once detached.
  if [[ -z "$(vol_attached appdata)" ]]; then
    ok "volume detached after stop"
  else
    bad "volume still attached after stop ($(vol_attached appdata))"
  fi
  # grow while stopped.
  if gerr=$(run volume grow appdata --size "$SZ_GROWN" 2>&1); then
    ok "volume grow appdata (while stopped)"
  else
    bad "grow appdata: $gerr"
  fi
  # start -> boots fresh, sees the new size.
  run app start growapp >/dev/null 2>&1 && ok "app start returned" || bad "app start"
  if wait_phase growapp running 600; then
    marker=$(run app exec growapp -- cat /data/marker 2>/dev/null | tr -d '\r\n')
    [[ "$marker" == "recipe-ok" ]] && ok "data intact after stop->grow->start (marker=$marker)" \
      || bad "marker='$marker' want 'recipe-ok'"
    after=$(app_kb growapp /data)
    if [[ -n "$before" && -n "$after" ]] && (( after > before * 3 )); then
      ok "app's guest filesystem grew (${before}K -> ${after}K blocks)"
    else
      bad "app guest fs did not grow (${before}K -> ${after}K blocks)"
    fi
  else
    bad "app did not come back up after start"
  fi
else
  bad "volume app never booted"
fi

echo "==============================================================="
echo " result: $pass passed, $fail failed"
echo "==============================================================="
[[ $fail -eq 0 ]] || exit 1
