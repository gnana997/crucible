#!/usr/bin/env bash
#
# smoke_kitchen_sink.sh — the integration test: many features COMPOSED on one
# stateful workload, which no isolated smoke proves. A postgres app on an
# ENCRYPTED volume, its password from a SECRET bundle, driven through the full
# stateful lifecycle:
#
#   secret + encrypted volume + postgres app
#     → write data
#     → sleep (ciphertext at rest) → wake (data intact)
#     → full backup → more data → incremental backup
#     → stop → grow the volume → start (data intact, bigger)
#     → restore the incremental chain into a NEW volume → boot a 2nd postgres → data intact
#
# The point is the INTERACTIONS: encryption × snapshot-sleep × backup × grow ×
# restore all on the same live database. Individual features have their own
# smokes; this proves they hold together.
#
# Requires: root + KVM, firecracker + jailer + vmlinux + a rootfs whose agent
# has /mount, mkfs.ext4, resize2fs, e2fsck, cryptsetup, and a pullable
# postgres image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_kitchen_sink.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
LISTEN="${LISTEN:-127.0.0.1:7918}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-kitchen}"
MARKER="KITCHENSINKMARKER42"   # distinctive, so we can prove it's ciphertext at rest

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible kitchen-sink integration smoke"
echo "==============================================================="

[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
for t in mkfs.ext4 resize2fs e2fsck cryptsetup; do command -v "$t" >/dev/null || { echo "error: $t needed" >&2; exit 2; }; done
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

echo "== 01 prepare work root ($MOUNT)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
head -c 32 /dev/urandom | base64 > "$MOUNT/volkey"
head -c 32 /dev/urandom | base64 > "$MOUNT/secretkey"
printf 'POSTGRES_PASSWORD=smoke\n' > "$MOUNT/pg.env"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" != "1" ]] && rm -rf "$MOUNT"
}
trap cleanup EXIT

echo "== 02 start daemon (encryption + secrets keys)"
"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --volume-dir "$MOUNT/volumes" --volume-encrypt-key-file "$MOUNT/volkey" \
  --secrets-key-file "$MOUNT/secretkey" --app-db "$MOUNT/apps.db" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
echo "   daemon healthy (pid $DAEMON_PID)"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_phase() { local want="$2" t="${3:-800}"; for _ in $(seq 1 "$t"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.05; done; return 1; }
# -q suppresses command tags (CREATE TABLE / INSERT 0 1) so a multi-statement
# query returns only its SELECT output.
pgq() { run app exec "$1" -- env PGPASSWORD=smoke psql -q -h 127.0.0.1 -U postgres -tAc "$2" 2>/dev/null | tr -d '\r\n '; }
pg_wait_ready() {
  for _ in $(seq 1 300); do
    [[ "$(run app exec "$1" -- pg_isready -h 127.0.0.1 2>/dev/null)" == *"accepting connections"* ]] && return 0
    sleep 0.2
  done
  return 1
}

echo "== 03 secret + encrypted volume + a postgres app that composes them"
run secret set pgsecret --from-env-file "$MOUNT/pg.env" >/dev/null 2>&1 \
  && ok "created secret bundle pgsecret" || bad "secret set"
run volume create pgdata --encrypt --size $((256*1024*1024)) >/dev/null 2>&1 \
  && ok "created ENCRYPTED volume pgdata" || bad "volume create --encrypt"
run app create pg --image "$PG_IMAGE" --restart always \
  --volume pgdata:/var/lib/postgresql/data --secrets pgsecret \
  --health tcp:5432 --memory 512 \
  -e PGDATA=/var/lib/postgresql/data/pgdata >/dev/null 2>&1
if wait_phase pg running 900 && pg_wait_ready pg; then
  ok "encrypted+secret postgres booted (password from the secret bundle)"
else
  bad "postgres never came up (is $PG_IMAGE pullable? enough RAM?): $(run app get pg 2>/dev/null | head -c 300)"
  echo " result: $pass passed, $((fail+1)) failed"; exit 1
fi

echo "== 04 write data through the composed stack"
if [[ "$(pgq pg "CREATE TABLE t(x text); INSERT INTO t VALUES ('$MARKER'); SELECT 'ok';")" == "ok" ]]; then
  ok "wrote a row (marker) via secret-authenticated psql"
else
  bad "write failed"
fi

echo "== 05 sleep → ciphertext at rest → wake → data intact"
run app sleep pg >/dev/null 2>&1
if wait_phase pg asleep 400; then
  ok "app slept (snapshot, VMM stopped, encrypted device closed)"
  # The marker must NOT appear in the raw LUKS container while at rest.
  if grep -aqs "$MARKER" "$MOUNT"/volumes/pgdata.img; then
    bad "MARKER found in plaintext in the volume container (not encrypted!)"
  else
    ok "volume container is ciphertext at rest (marker absent from the raw file)"
  fi
else
  bad "app did not sleep"
fi
run app wake pg >/dev/null 2>&1
if wait_phase pg running 400 && pg_wait_ready pg; then
  [[ "$(pgq pg "SELECT x FROM t LIMIT 1;")" == "$MARKER" ]] \
    && ok "data survived sleep→wake on the encrypted volume" || bad "data lost across sleep/wake"
else
  bad "app did not wake"
fi

echo "== 06 full backup → more data → incremental backup"
run app sleep pg >/dev/null 2>&1; wait_phase pg asleep 400
FULL=$(run volume backup pgdata 2>/dev/null | tr -d '\r\n')
[[ -n "$FULL" ]] && ok "full backup of the (slept) encrypted volume ($FULL)" || bad "full backup"
run app wake pg >/dev/null 2>&1; wait_phase pg running 400; pg_wait_ready pg
pgq pg "INSERT INTO t VALUES ('SECOND');" >/dev/null
run app sleep pg >/dev/null 2>&1; wait_phase pg asleep 400
INC=$(run volume backup pgdata --parent "$FULL" 2>/dev/null | tr -d '\r\n')
[[ -n "$INC" ]] && ok "incremental backup against the full ($INC)" || bad "incremental backup"

echo "== 07 stop → grow the volume → start → data intact + bigger"
run app stop pg >/dev/null 2>&1
if [[ -z "$(curl -s "$BASE_URL/volumes/pgdata" 2>/dev/null | grep -o '"attached_to"')" ]]; then
  ok "volume detached after app stop"
else
  bad "volume still attached after stop"
fi
run volume grow pgdata --size $((768*1024*1024)) >/dev/null 2>&1 \
  && ok "grew the encrypted volume 256M→768M while stopped" || bad "grow"
run app start pg >/dev/null 2>&1
if wait_phase pg running 900 && pg_wait_ready pg; then
  got1=$(pgq pg "SELECT x FROM t WHERE x='$MARKER';")
  got2=$(pgq pg "SELECT x FROM t WHERE x='SECOND';")
  [[ "$got1" == "$MARKER" && "$got2" == "SECOND" ]] \
    && ok "both rows intact after stop→grow→start" || bad "data lost across grow (m1='$got1' m2='$got2')"
  dfk=$(run app exec pg -- df -Pk /var/lib/postgresql/data 2>/dev/null | awk 'NR==2{print $2}')
  [[ -n "$dfk" ]] && (( dfk > 400000 )) && ok "guest sees the grown volume (${dfk}K)" || bad "guest fs did not grow (${dfk}K)"
else
  bad "postgres did not come back after start"
fi

echo "== 08 restore the incremental chain into a NEW volume → 2nd postgres → data intact"
run volume restore --from "$INC" --to pgrestored >/dev/null 2>&1 \
  && ok "restored the incremental tip → pgrestored" || bad "restore"
run app create pg2 --image "$PG_IMAGE" --restart always \
  --volume pgrestored:/var/lib/postgresql/data --secrets pgsecret \
  --health tcp:5432 --memory 512 \
  -e PGDATA=/var/lib/postgresql/data/pgdata >/dev/null 2>&1
if wait_phase pg2 running 900 && pg_wait_ready pg2; then
  # The incremental tip was taken AFTER the SECOND insert, so both rows must be present.
  g1=$(pgq pg2 "SELECT x FROM t WHERE x='$MARKER';")
  g2=$(pgq pg2 "SELECT x FROM t WHERE x='SECOND';")
  [[ "$g1" == "$MARKER" && "$g2" == "SECOND" ]] \
    && ok "restored database (from the incremental chain) has both rows" \
    || bad "restored data wrong (m1='$g1' m2='$g2')"
else
  bad "2nd postgres on the restored volume did not boot"
fi

echo "==============================================================="
echo " result: $pass passed, $fail failed"
echo "==============================================================="
[[ $fail -eq 0 ]] || exit 1
