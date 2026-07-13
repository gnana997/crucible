#!/usr/bin/env bash
#
# bench_serverless.sh — the v0.6.2 headline number: how fast a serverless
# postgres on a volume comes back, snapshot-wake vs cold-boot.
#
# Measures, for the same tiny postgres (one row) on a persistent volume:
#   - WARM  (v0.6.2 snapshot-wake): app sleep → app wake → first query answered.
#           Also records the daemon's precise restore latency (last_wake_latency_ms).
#   - COLD  (v0.6.1 behavior):  app rm → app create (same volume) → first query
#           answered. The volume is already initialized, so this is boot + WAL
#           recovery, NOT initdb — the fair floor of a cold start.
# Reports median / mean / min / max (ms) for each, and the speedup.
#
# Run it on the same box + filesystem you benchmark crucible on (put --work-base
# / --volume-dir on ext4 or btrfs to see the storage floor/ceiling; wake barely
# cares, but report it). Needs: root + KVM, firecracker + jailer + vmlinux + a
# rootfs, mkfs.ext4, psql on the host, postgres:16-alpine pullable, a default
# egress iface.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        [SAMPLES=10] [MOUNT=/var/lib/crucible-bench-serverless] \
#        scripts/bench_serverless.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7912}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-bench-serverless}"
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
HP_PG="${HP_PG:-7931}"
SAMPLES="${SAMPLES:-10}"   # measured samples per path; 2 warmups are discarded
WARMUPS=2

echo "==============================================================="
echo " crucible serverless wake benchmark (v0.6.2)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null || { echo "error: mkfs.ext4 needed" >&2; exit 2; }
command -v psql >/dev/null || { echo "error: psql needed on the host (postgresql-client)" >&2; exit 2; }
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>)" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

echo "== prepare work root ($MOUNT), egress $EGRESS, samples $SAMPLES (+$WARMUPS warmup)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
FS="$(stat -f -c %T "$MOUNT" 2>/dev/null || echo '?')"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --volume-dir "$MOUNT/volumes" --app-db "$MOUNT/apps.db" \
  --network-egress-iface "$EGRESS" --log-format json --log-level warn >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
echo "   daemon healthy (pid $DAEMON_PID), work-base fs: $FS"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_phase() { local want="$2" tries="${3:-400}"; for _ in $(seq 1 "$tries"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.05; done; return 1; }
wake_ms() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"last_wake_latency_ms":[0-9]*' | grep -o '[0-9]*'; }
now_ns() { date +%s%N; }
# psql_ready POLL: from now, poll a trivial query until it answers; echo elapsed ms.
psql_ready() {
  local t0="$1"
  while true; do
    PGPASSWORD=bench psql -h 127.0.0.1 -p "$HP_PG" -U postgres -tAc 'SELECT 1' >/dev/null 2>&1 && break
    sleep 0.02
  done
  echo $(( ( $(now_ns) - t0 ) / 1000000 ))
}

# stats NAME v1 v2 …  — prints median/mean/min/max and stashes the median in MEDIAN.
MEDIAN=0
stats() {
  local name="$1"; shift
  local sorted n mid sum=0 v
  sorted=$(printf '%s\n' "$@" | sort -n)
  n=$#
  for v in "$@"; do sum=$((sum + v)); done
  mid=$(printf '%s\n' "$sorted" | awk "NR==int(($n+1)/2)")
  MEDIAN=$mid
  printf "  %-34s median %5s ms   mean %5s ms   min %5s   max %5s\n" \
    "$name" "$mid" "$((sum / n))" "$(printf '%s\n' "$sorted" | head -1)" "$(printf '%s\n' "$sorted" | tail -1)"
}

# ---- one-time setup: a tiny postgres on a volume (initdb runs once here) -----
echo "== setup: boot postgres on a volume + write one row (initdb, one time)"
run app rm pg >/dev/null 2>&1 || true; run volume rm pgbench >/dev/null 2>&1 || true
WARM=$(run run "$PG_IMAGE" --pull missing -- true 2>/dev/null); run rm "$WARM" >/dev/null 2>&1 || true
run app create pg --image "$PG_IMAGE" -p "$HP_PG:5432" --restart always --health "tcp:5432" \
  --memory 512 --volume pgbench:/var/lib/postgresql/data \
  -e POSTGRES_PASSWORD=bench -e PGDATA=/var/lib/postgresql/data/pgdata >/dev/null 2>&1
wait_phase pg running 600 || { echo "postgres never booted"; run app get pg; exit 3; }
# wait until it actually answers, then seed a row.
psql_ready "$(now_ns)" >/dev/null
PGPASSWORD=bench psql -h 127.0.0.1 -p "$HP_PG" -U postgres -c 'CREATE TABLE IF NOT EXISTS t(x int)' -c 'INSERT INTO t VALUES(1)' >/dev/null 2>&1
echo "   postgres ready, row seeded"

# ---- WARM: v0.6.2 snapshot-wake -------------------------------------------------
echo "== WARM  (v0.6.2 snapshot-wake): app sleep → wake → first query answered"
warm_e2e=(); warm_restore=()
for i in $(seq 1 $((SAMPLES + WARMUPS))); do
  run app sleep pg >/dev/null 2>&1
  wait_phase pg asleep 200 || { echo "   sample $i: did not sleep"; continue; }
  t0=$(now_ns)
  run app wake pg >/dev/null 2>&1
  wait_phase pg running 200 || { echo "   sample $i: did not wake"; continue; }
  e2e=$(psql_ready "$t0")
  r=$(wake_ms pg); r=${r:-0}
  if [[ $i -gt $WARMUPS ]]; then warm_e2e+=("$e2e"); warm_restore+=("$r"); fi
  printf "   sample %2d: restore %4s ms   e2e %4s ms%s\n" "$i" "$r" "$e2e" "$([[ $i -le $WARMUPS ]] && echo '  (warmup)')"
done

# ---- COLD: v0.6.1 cold-boot (destroy → cold-create, volume already init'd) --
echo "== COLD  (cold-boot): app rm → app create (same volume) → first query answered"
cold_e2e=()
for i in $(seq 1 $((SAMPLES + WARMUPS))); do
  run app rm pg >/dev/null 2>&1
  for _ in $(seq 1 100); do [[ -z "$(app_phase pg)" ]] && break; sleep 0.05; done
  t0=$(now_ns)
  run app create pg --image "$PG_IMAGE" -p "$HP_PG:5432" --restart always --health "tcp:5432" \
    --memory 512 --volume pgbench:/var/lib/postgresql/data \
    -e POSTGRES_PASSWORD=bench -e PGDATA=/var/lib/postgresql/data/pgdata >/dev/null 2>&1
  wait_phase pg running 600 || { echo "   sample $i: did not boot"; continue; }
  e2e=$(psql_ready "$t0")
  if [[ $i -gt $WARMUPS ]]; then cold_e2e+=("$e2e"); fi
  printf "   sample %2d: cold-boot e2e %5s ms%s\n" "$i" "$e2e" "$([[ $i -le $WARMUPS ]] && echo '  (warmup)')"
done

run app rm pg >/dev/null 2>&1 || true; run volume rm pgbench >/dev/null 2>&1 || true

# ---- report -----------------------------------------------------------------
echo "==============================================================="
echo " results  (postgres:16-alpine, 512 MiB, 1-row DB, work-base fs: $FS)"
echo "==============================================================="
stats "WARM restore (last_wake_latency)"  "${warm_restore[@]}"; warm_r=$MEDIAN
stats "WARM end-to-end (wake → query)"     "${warm_e2e[@]}";     warm_e=$MEDIAN
stats "COLD end-to-end (create → query)"   "${cold_e2e[@]}";     cold_e=$MEDIAN
echo "---------------------------------------------------------------"
[[ "$warm_e" -gt 0 ]] && printf " speedup (cold ÷ warm, end-to-end): %sx\n" "$(( cold_e / warm_e ))"
echo "==============================================================="
