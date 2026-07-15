#!/usr/bin/env bash
#
# smoke_volume_encrypt.sh — end-to-end check for encrypted volumes (encryption
# at rest with per-volume LUKS keys + crypto-shred).
#
# Runs a real PostgreSQL on an ENCRYPTED volume and proves the security contract
# the feature exists for:
#
#   03  `volume create --encrypt` makes a LUKS container; `volume ls` shows it
#       encrypted; the on-disk .img is a LUKS header, not plaintext ext4. A
#       --no-encrypt volume is plaintext (control).
#   04  postgres boots on the encrypted volume and a distinctive row is committed.
#   05  while the app SLEEPS, the decrypted /dev/mapper device is CLOSED (the data
#       is ciphertext at rest during sleep), and the on-disk .img does NOT contain
#       the row's plaintext.
#   06  a backup of the encrypted volume (ciphertext) restores to a new volume.
#   07  waking the app re-opens the device and the row is intact.
#   08  a second postgres on the RESTORED volume reads the same row (restore
#       re-wrapped the key correctly).
#   09  the row survives a daemon restart (key file reloaded, encrypted volume
#       re-attached).
#   10  `volume shred` destroys the key and the backing file — the data is
#       permanently unrecoverable; shred is refused on a plaintext volume.
#
# Requires: root + KVM, firecracker + jailer + vmlinux + a rootfs whose agent
# has /mount, cryptsetup, mkfs.ext4, curl, a pullable postgres image. The
# volume-dir sits on the SAME filesystem as the chroot base (volumes hardlink
# into the jail; the encrypted container is opened host-side and its device node
# is mknod'd into the jail).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_volume_encrypt.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7924}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-encvol}"
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
MARKER="CRUCIBLE_ENC_ROW_9f3a2c7e_do_not_leak"   # the plaintext we hunt for on disk

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible encrypted-volume smoke (postgres at rest)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer + cryptsetup)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v cryptsetup >/dev/null || { echo "error: cryptsetup needed (install cryptsetup)" >&2; exit 2; }
command -v mkfs.ext4  >/dev/null || { echo "error: mkfs.ext4 needed (e2fsprogs)" >&2; exit 2; }
command -v curl       >/dev/null || { echo "error: curl needed" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

echo "== 01 prepare work root ($MOUNT)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
VOL_DIR="$MOUNT/volumes"
KEY_FILE="$MOUNT/volume.key"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  # Close any mapper devices this smoke left open before wiping the dir.
  for m in /dev/mapper/crucible-vol-* /dev/mapper/crucible-fmt-*; do
    [[ -e "$m" ]] && cryptsetup close "$(basename "$m")" 2>/dev/null || true
  done
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
    --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
    --volume-dir "$VOL_DIR" --volume-default-size $((512*1024*1024)) \
    --volume-encrypt-key-file "$KEY_FILE" \
    --app-db "$MOUNT/apps.db" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never became healthy"; tail -20 "$DAEMON_LOG"; exit 3
}

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
phase_of() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1; }
wait_ph() { for _ in $(seq 1 240); do [[ "$(phase_of "$1")" == "\"phase\":\"$2\"" ]] && return 0; sleep 0.5; done; return 1; }
# psql over the local trust socket inside the guest; prints the (trimmed) result.
psql_q() { run app exec "$1" -- psql -U postgres -tAc "$2" 2>/dev/null | tr -d '[:space:]'; }
pg_ready() { for _ in $(seq 1 120); do run app exec "$1" -- pg_isready -U postgres >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }

echo "== 02 start daemon (per-volume encryption, key generated on first run)"
start_daemon
grep -q "generated a new volume encryption master key" "$DAEMON_LOG" && ok "master key generated + back-up warning logged" || bad "no key-generation warning"
grep -q "volume encryption enabled" "$DAEMON_LOG" && ok "volume encryption enabled" || bad "volume encryption not enabled"
[[ -f "$KEY_FILE" ]] && [[ "$(stat -c '%a' "$KEY_FILE")" == "600" ]] && ok "key file written 0600" || bad "key file missing/not 0600"

echo "== 03 create an encrypted volume + a plaintext control"
run volume create pgdata --encrypt --size 512M >/dev/null 2>&1 && ok "volume create --encrypt" || bad "volume create --encrypt failed"
run volume create plain1 --no-encrypt --size 128M >/dev/null 2>&1 && ok "volume create --no-encrypt" || bad "volume create --no-encrypt failed"
run volume ls 2>/dev/null | awk '$1=="pgdata"{print $3}' | grep -qx yes && ok "'volume ls' shows pgdata ENCRYPTED=yes" || bad "pgdata not shown encrypted"
run volume ls 2>/dev/null | awk '$1=="plain1"{print $3}' | grep -qx no && ok "'volume ls' shows plain1 ENCRYPTED=no" || bad "plain1 not shown plaintext"
# On-disk: the encrypted container is a LUKS header; the plaintext one is not.
head -c 6 "$VOL_DIR/pgdata.img" | grep -aq "LUKS" && ok "pgdata.img on disk is a LUKS container" || bad "pgdata.img is not LUKS"
head -c 6 "$VOL_DIR/plain1.img" | grep -aq "LUKS" && bad "plain1.img is unexpectedly LUKS" || ok "plain1.img is plaintext (control)"

echo "== 04 boot postgres on the encrypted volume + commit a row"
run app create pg --image "$PG_IMAGE" --volume pgdata:/var/lib/postgresql/data \
  --env POSTGRES_PASSWORD=smoke --env PGDATA=/var/lib/postgresql/data/pgdata \
  --vcpus 1 --memory 512 --restart always >/dev/null 2>&1
if wait_ph pg running && pg_ready pg; then
  ok "postgres running on the encrypted volume"
else
  bad "postgres never became ready"; run app get pg 2>&1 | head -c 400; echo; tail -25 "$DAEMON_LOG"; exit 1
fi
psql_q pg "CREATE TABLE t(v text); INSERT INTO t VALUES('$MARKER'); CHECKPOINT;" >/dev/null
GOT=$(psql_q pg "SELECT v FROM t LIMIT 1;")
[[ "$GOT" == "$MARKER" ]] && ok "row committed + read back in-guest" || bad "row read-back = '$GOT'"
# The live device must be open while the app runs.
[[ -e /dev/mapper/crucible-vol-pgdata ]] && ok "decrypted device open while running" || bad "no mapper device while running"

echo "== 05 sleep: device closes + on-disk is ciphertext"
run app sleep pg >/dev/null 2>&1
if wait_ph pg asleep; then
  ok "postgres app snapshot-slept"
else
  bad "app did not sleep: phase=$(phase_of pg)"
fi
# F1: a slept encrypted volume must NOT stay decrypted/online on the host.
[[ -e /dev/mapper/crucible-vol-pgdata ]] && bad "SECURITY: mapper device left OPEN while asleep" || ok "decrypted device closed while asleep (encrypted at rest)"
# The committed row must not appear in the raw LUKS container.
grep -aq "$MARKER" "$VOL_DIR/pgdata.img" && bad "SECURITY: plaintext row found in the on-disk container" || ok "on-disk container is ciphertext (row absent)"

echo "== 06 back up the encrypted volume (slept = quiescent) + restore to a new volume"
run volume backup pgdata >/dev/null 2>&1 && ok "backup of encrypted volume taken" || bad "volume backup failed"
BID=$(run volume backup ls pgdata 2>/dev/null | awk 'NR==2{print $1}')
[[ -n "$BID" ]] && ok "backup id: $BID" || bad "no backup id listed"
run volume restore --from "$BID" --to pgrestored >/dev/null 2>&1 && ok "restore to pgrestored" || bad "restore failed"
run volume ls 2>/dev/null | awk '$1=="pgrestored"{print $3}' | grep -qx yes && ok "restored volume is encrypted" || bad "restored volume not encrypted"

echo "== 07 wake: device re-opens + row intact"
run app wake pg >/dev/null 2>&1
if wait_ph pg running && pg_ready pg; then
  GOT=$(psql_q pg "SELECT v FROM t LIMIT 1;")
  [[ "$GOT" == "$MARKER" ]] && ok "woke, re-opened device, row intact" || bad "post-wake row = '$GOT'"
else
  bad "app did not wake: $(run app get pg 2>/dev/null | head -c 300)"
fi

echo "== 08 a second postgres on the RESTORED volume reads the same row"
run app create pg2 --image "$PG_IMAGE" --volume pgrestored:/var/lib/postgresql/data \
  --env POSTGRES_PASSWORD=smoke --env PGDATA=/var/lib/postgresql/data/pgdata \
  --vcpus 1 --memory 512 --restart always >/dev/null 2>&1
if wait_ph pg2 running && pg_ready pg2; then
  GOT=$(psql_q pg2 "SELECT v FROM t LIMIT 1;")
  [[ "$GOT" == "$MARKER" ]] && ok "restored volume opened + row present (key re-wrapped correctly)" || bad "restored row = '$GOT'"
else
  bad "pg2 on restored volume never ready"; run app get pg2 2>&1 | head -c 300
fi
run app rm pg2 >/dev/null 2>&1 || true

echo "== 09 row survives a daemon restart (key file reloaded)"
kill -TERM "$DAEMON_PID" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null
start_daemon
if wait_ph pg running && pg_ready pg; then
  GOT=$(psql_q pg "SELECT v FROM t LIMIT 1;")
  [[ "$GOT" == "$MARKER" ]] && ok "encrypted volume re-attached after restart, row intact" || bad "post-restart row = '$GOT'"
else
  # If the app was running at restart it re-adopts + reconciles; give it a wake.
  run app wake pg >/dev/null 2>&1 || true
  if wait_ph pg running && pg_ready pg; then
    GOT=$(psql_q pg "SELECT v FROM t LIMIT 1;")
    [[ "$GOT" == "$MARKER" ]] && ok "encrypted volume re-attached after restart, row intact" || bad "post-restart row = '$GOT'"
  else
    bad "pg did not come back after restart"
  fi
fi

echo "== 10 crypto-shred: key destroyed, data unrecoverable"
run app rm pg >/dev/null 2>&1 || true   # release the single-writer guard
sleep 1
# Shred is refused on a plaintext volume.
if run volume shred plain1 >/dev/null 2>&1; then bad "shred of a plaintext volume was NOT refused"; else ok "shred refused on plaintext volume"; fi
run volume shred pgdata >/dev/null 2>&1 && ok "volume shred pgdata" || bad "volume shred failed"
[[ -e "$VOL_DIR/pgdata.img" ]] && bad "shredded backing file still present" || ok "shredded backing file removed"
run volume ls 2>/dev/null | awk '{print $1}' | grep -qx pgdata && bad "shredded volume still listed" || ok "shredded volume gone from the catalog"

run volume rm plain1 pgrestored >/dev/null 2>&1 || true

echo "==============================================================="
echo " encrypted-volume smoke: $pass passed, $fail failed"
echo " transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ $fail -eq 0 ]]
