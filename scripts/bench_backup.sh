#!/usr/bin/env bash
#
# bench_backup.sh: the v0.6.3 headline. How disruptive is a LIVE volume backup?
#
# A live backup FIFREEZEs the volume's filesystem for the (reflink, O(1)) copy,
# then thaws. This measures how long a running database is actually paused. It
# runs a postgres on a btrfs volume under a continuous psql INSERT load (every
# autocommit fsyncs WAL to the volume fs, so an in-flight INSERT blocks for exactly
# the freeze window), takes N live backups mid-load, and reports:
#   - backup latency: wall time of each `volume backup` call (the operation cost);
#   - freeze blip: the MAX INSERT latency during the run (the guest-observed pause)
#     vs the baseline INSERT latency;
#   - failed transactions: should be ZERO (no downtime, just a brief pause).
#
# A reflink work root is REQUIRED (live backup is refused on ext4), so this sets
# up a btrfs loopback. Needs: root + KVM, firecracker + jailer + vmlinux + rootfs,
# mkfs.btrfs, mkfs.ext4, and psql on the host (no pgbench), postgres:16-alpine.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        [SAMPLES=8] scripts/bench_backup.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7914}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/mnt/crucible-bkpbench}"
IMG="${IMG:-/var/lib/crucible-bkpbench.img}"
IMG_SIZE="${IMG_SIZE:-6G}"
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
HP_PG="${HP_PG:-7941}"
SAMPLES="${SAMPLES:-8}"    # live backups taken during the load window (2s apart)
WRITERS="${WRITERS:-8}"    # concurrent psql INSERT writers (the load level)

echo "==============================================================="
echo " crucible live-backup benchmark (v0.6.3, freeze window)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer + loop)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
for t in mkfs.btrfs mkfs.ext4 psql; do command -v "$t" >/dev/null || { echo "error: need $t on the host" >&2; exit 2; }; done
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>)" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

echo "== 01 btrfs work root (reflink; required for live backup)"
umount "$MOUNT" 2>/dev/null || true
truncate -s "$IMG_SIZE" "$IMG"; mkfs.btrfs -q -f "$IMG"
mkdir -p "$MOUNT"; mount -o loop "$IMG" "$MOUNT"
findmnt -no FSTYPE "$MOUNT" | grep -q btrfs || { echo "error: $MOUNT is not btrfs" >&2; exit 3; }
mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  if [[ "${KEEP:-0}" != "1" ]]; then umount "$MOUNT" 2>/dev/null || true; rm -f "$IMG"; rm -rf "$MOUNT"; fi
}
trap cleanup EXIT

echo "== 02 start daemon (--volume-dir + backups on btrfs → reflink)"
"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --volume-dir "$MOUNT/volumes" --app-db "$MOUNT/apps.db" \
  --network-egress-iface "$EGRESS" --log-format json --log-level warn >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
echo "   daemon healthy (pid $DAEMON_PID)"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_phase() { local want="$2" t="${3:-600}"; for _ in $(seq 1 "$t"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.05; done; return 1; }
pgq() { PGPASSWORD=bench psql -h 127.0.0.1 -p "$HP_PG" -U postgres -tAc "$1" 2>/dev/null; }

echo "== 03 boot postgres on a volume + prepare the load table"
run app rm pgb >/dev/null 2>&1 || true; run volume rm pgbvol >/dev/null 2>&1 || true
WARM=$(run run "$PG_IMAGE" --pull missing -- true 2>/dev/null); run rm "$WARM" >/dev/null 2>&1 || true
run app create pgb --image "$PG_IMAGE" -p "$HP_PG:5432" --restart always --health "tcp:5432" \
  --memory 512 --volume pgbvol:/var/lib/postgresql/data \
  -e POSTGRES_PASSWORD=bench -e PGDATA=/var/lib/postgresql/data/pgdata >/dev/null 2>&1
wait_phase pgb running 900 || { echo "postgres never booted"; run app get pgb; exit 3; }
for _ in $(seq 1 300); do [[ "$(pgq 'SELECT 1')" == "1" ]] && break; sleep 0.1; done
pgq 'CREATE TABLE IF NOT EXISTS bench_t(v int, pad text)' >/dev/null
# A big stream of autocommit INSERTs with per-statement timing, each writing a
# padded row so commits move real WAL to the volume fs. An insert that lands
# during a freeze blocks for exactly the freeze window; `\timing` reports each
# insert's latency in ms. Run by $WRITERS concurrent psql sessions for real load.
{ echo '\timing on'; awk 'BEGIN{for(i=0;i<500000;i++)print "INSERT INTO bench_t VALUES (1, repeat('"'"'x'"'"',512));"}'; } > "$MOUNT/load.sql"
echo "   postgres ready"

# ---- 04 concurrent INSERT load; take N live backups mid-load ----------------
echo "== 04 drive $WRITERS concurrent INSERT writers, take $SAMPLES live backups mid-load"
now_ns() { date +%s%N; }
LOAD_START=$(now_ns)
LOAD_PIDS=()
for w in $(seq 1 "$WRITERS"); do
  PGPASSWORD=bench psql -h 127.0.0.1 -p "$HP_PG" -U postgres -q -f "$MOUNT/load.sql" \
    >"$MOUNT/load.$w.out" 2>&1 &
  LOAD_PIDS+=($!)
done

sleep 3   # let the load ramp up
bk=()
for i in $(seq 1 "$SAMPLES"); do
  kill -0 "${LOAD_PIDS[0]}" 2>/dev/null || break   # load streams exhausted early
  t0=$(now_ns)
  BID=$(run volume backup pgbvol 2>/dev/null | tr -d '\r\n')
  ms=$(( ( $(now_ns) - t0 ) / 1000000 ))
  if [[ -n "$BID" ]]; then bk+=("$ms"); printf "   backup %d: %4s ms  (%s)\n" "$i" "$ms" "$BID"; fi
  sleep 2
done
for p in "${LOAD_PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
wait "${LOAD_PIDS[@]}" 2>/dev/null || true   # only the writers, NOT the daemon
LOAD_SECS=$(( ( $(now_ns) - LOAD_START ) / 1000000000 )); [[ $LOAD_SECS -lt 1 ]] && LOAD_SECS=1

# ---- 05 analyse -------------------------------------------------------------
cat "$MOUNT"/load.*.out > "$MOUNT/load.all" 2>/dev/null
grep -Eq '^Time:' "$MOUNT/load.all" || { echo "error: no timed inserts (load failed)"; tail -20 "$MOUNT"/load.*.out; exit 3; }
# psql `\timing` prints "Time: <ms> ms" per statement; column 2 is the latency (ms).
LAT_SORTED=$(grep -E '^Time:' "$MOUNT/load.all" | awk '{print $2}' | sort -n)
NX=$(printf '%s\n' "$LAT_SORTED" | grep -c .)
AVG_MS=$(printf '%s\n' "$LAT_SORTED" | awk '{s+=$1;n++} END{printf "%.2f",(n?s/n:0)}')
MAX_MS=$(printf '%s\n' "$LAT_SORTED" | tail -1)
P99_MS=$(printf '%s\n' "$LAT_SORTED" | awk -v n="$NX" 'NR>=int(n*0.99){print;exit}')
FAILED=$(grep -c '^ERROR' "$MOUNT/load.all")
TPS=$(( NX / LOAD_SECS ))

# backup latency median
bmed=0; bmin="?"; bmax="?"
if [[ ${#bk[@]} -gt 0 ]]; then
  sorted=$(printf '%s\n' "${bk[@]}" | sort -n); bmed=$(printf '%s\n' "$sorted" | awk "NR==int((${#bk[@]}+1)/2)")
  bmin=$(printf '%s\n' "$sorted" | head -1); bmax=$(printf '%s\n' "$sorted" | tail -1)
fi

run app rm pgb >/dev/null 2>&1 || true; run volume rm pgbvol >/dev/null 2>&1 || true

echo "==============================================================="
echo " results  (postgres:16-alpine, 512 MiB, btrfs/reflink volume)"
echo "==============================================================="
printf " load                       %d writers, ~%d inserts/sec (512B rows)\n" "$WRITERS" "$TPS"
printf " live backup call latency   median %s ms   (min %s, max %s, n=%d)\n" "$bmed" "$bmin" "$bmax" "${#bk[@]}"
printf " INSERT latency (baseline)  avg %s ms   p99 %s ms\n" "$AVG_MS" "$P99_MS"
printf " INSERT latency (MAX)       %s ms   <- guest freeze blip during a backup\n" "$MAX_MS"
printf " inserts                    %d total, %d failed\n" "$NX" "$FAILED"
echo "---------------------------------------------------------------"
[[ "$FAILED" == "0" ]] && echo " no downtime: 0 failed inserts under load across ${#bk[@]} live backups" \
                       || echo " WARNING: $FAILED failed inserts"
echo "==============================================================="
