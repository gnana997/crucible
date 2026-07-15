#!/usr/bin/env bash
#
# smoke_usage.sh — persistent usage metrics (v0.7.1), realistic workload.
#
# Durable per-app usage counters that survive a daemon restart. This drives a
# real, multi-app workload and checks the ledger for ACCURACY and correct
# behaviour under the messy cases — not just that a number is non-zero:
#
#   01  daemon up (proxy + apps + volumes + a fast --usage-interval)
#   02  two apps run: web (2 vCPU / 256 MiB) and api (1 vCPU / 128 MiB)
#   03  ATTRIBUTION under concurrency: 30 requests to web + 12 to api, fired
#       concurrently, are counted EXACTLY and to the right app (race-free)
#   04  ACCURACY: over a measured awake window, Δcompute ≈ vCPUs × seconds and
#       Δmemory ≈ MiB × seconds (within a band) — the ledger tracks wall-clock
#   05  sleep FREEZES compute: a slept app's compute doesn't move
#   06  wake RESUMES accrual: compute climbs again and new requests are counted
#   07  STORAGE dimension: a volume app accrues storage, and KEEPS accruing while
#       ASLEEP (a slept app still holds its disk) even as its compute is frozen
#   08  a DELETED app's final usage is RETAINED (readable via GET /usage)
#   09  usage SURVIVES a daemon restart, and downtime is NOT back-filled
#
# Reads usage via `crucible app usage [<name>] -o json` (GET /usage,
# /apps/{name}/usage).
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# curl, python3, and a pullable nginx:alpine (or a cached copy).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_usage.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7918}"
BASE_URL="http://${LISTEN}"
HTTP_PORT="${HTTP_PORT:-8085}"        # ingress proxy (routes by Host)
PROXY_DOMAIN="${PROXY_DOMAIN:-apps.local}"
MOUNT="${MOUNT:-/var/lib/crucible-usage}"
IMAGE="${IMAGE:-nginx:alpine}"
USAGE_INTERVAL="${USAGE_INTERVAL:-2s}"

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible persistent usage metrics smoke (v0.7.1)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v curl >/dev/null && command -v python3 >/dev/null || { echo "error: curl + python3 needed" >&2; exit 2; }
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>)" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }

rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,images,logs,volumes}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"
APP_DB="$MOUNT/apps.db"

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
    --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
    --app-db "$APP_DB" --volume-dir "$MOUNT/volumes" --network-egress-iface "$EGRESS" \
    --proxy-listen "127.0.0.1:$HTTP_PORT" --proxy-domain "$PROXY_DOMAIN" \
    --usage-interval "$USAGE_INTERVAL" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0; sleep 0.2; done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; return 1
}
stop_daemon() {
  [[ -n "$DAEMON_PID" ]] || return 0
  kill -TERM "$DAEMON_PID" 2>/dev/null
  wait "$DAEMON_PID" 2>/dev/null; DAEMON_PID=""
}
cleanup() {
  stop_daemon
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

cli() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { cli app get "$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
wait_phase() { local want="$2" tries="${3:-240}"; for _ in $(seq 1 "$tries"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.5; done; return 1; }
# usage_field APP FIELD — a numeric field from `app usage APP -o json`.
usage_field() { cli app usage "$1" -o json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], 0))' "$2" 2>/dev/null; }
# hit APP N — fire N proxy requests to APP concurrently (each a fresh connection)
# and echo how many got a 2xx. Asserting usage == delivered isolates counting
# correctness (the race-free mutex) from any rare transient connection drop.
hit() {
  local host="$1.$PROXY_DOMAIN" n="$2" i got=0 dir pids=()
  dir="$(mktemp -d)"
  for ((i=0;i<n;i++)); do
    ( curl -s -o /dev/null -w '%{http_code}' -H "Host: $host" "http://127.0.0.1:$HTTP_PORT/" >"$dir/$i" ) &
    pids+=($!)
  done
  # wait ONLY on the curl PIDs — a bare `wait` here (this runs in a $(...) subshell)
  # would try to reap the inherited daemon job and spam "not a child of this shell".
  [[ ${#pids[@]} -gt 0 ]] && wait "${pids[@]}" 2>/dev/null
  for ((i=0;i<n;i++)); do [[ "$(cat "$dir/$i" 2>/dev/null)" == 2* ]] && got=$((got+1)); done
  rm -rf "$dir"; echo "$got"
}
# band VAL LO HI MSG — pass if LO <= VAL <= HI.
band() { python3 -c 'import sys; v,lo,hi=float(sys.argv[1]),float(sys.argv[2]),float(sys.argv[3]); sys.exit(0 if lo<=v<=hi else 1)' "$1" "$2" "$3"; }

echo "== 01 start daemon (proxy + apps + volumes + --usage-interval $USAGE_INTERVAL)"
start_daemon || exit 3
ok "daemon up"

echo "== 02 two apps: web (2 vCPU / 256 MiB) + api (1 vCPU / 128 MiB)"
cli app create web --image "$IMAGE" --pull missing --port 80 --restart always --health "http:80:/" --vcpus 2 --memory 256 >/dev/null 2>&1
cli app create api --image "$IMAGE" --pull missing --port 80 --restart always --health "http:80:/" --vcpus 1 --memory 128 >/dev/null 2>&1
if wait_phase web running && wait_phase api running; then ok "web + api running"; else bad "apps never ran (is $IMAGE pullable?)"; tail -20 "$DAEMON_LOG"; exit 1; fi

echo "== 03 concurrent request attribution is exact + per-app"
web_reqs="$(hit web 30)"   # cumulative expected count for web
api_reqs="$(hit api 12)"
sleep 3  # let the tick flush the request counters
rw="$(usage_field web requests)"; ra="$(usage_field api requests)"
[[ "$rw" == "$web_reqs" && "$web_reqs" -ge 28 ]] && ok "web counted all $web_reqs delivered requests exactly (of 30 concurrent)" \
  || bad "web requests = $rw, delivered = $web_reqs (want equal, ~30)"
[[ "$ra" == "$api_reqs" && "$api_reqs" -ge 11 ]] && ok "api counted its $api_reqs exactly — no cross-attribution from web" \
  || bad "api requests = $ra, delivered = $api_reqs (want equal, ~12)"

echo "== 04 compute + memory accrual tracks wall-clock (accuracy)"
c0="$(usage_field web compute_vcpu_seconds)"; m0="$(usage_field web memory_mib_seconds)"
sleep 8  # measured awake window
c1="$(usage_field web compute_vcpu_seconds)"; m1="$(usage_field web memory_mib_seconds)"
dc="$(python3 -c "print(float('$c1')-float('$c0'))")"; dm="$(python3 -c "print(float('$m1')-float('$m0'))")"
# 2 vCPU × ~8 s ≈ 16 vCPU·s; 256 MiB × ~8 s ≈ 2048 MiB·s. Wide band = catch
# gross errors (zero, 10× off) without being flaky on scheduling jitter.
band "$dc" 10 24    && ok "Δcompute over 8s = ${dc} vCPU·s (≈ 2 vCPU × 8 s)" || bad "Δcompute = ${dc} (want ~16, band 10..24)"
band "$dm" 1200 3000 && ok "Δmemory over 8s = ${dm} MiB·s (≈ 256 MiB × 8 s)"  || bad "Δmemory = ${dm} (want ~2048, band 1200..3000)"

echo "== 05 sleep FREEZES compute"
cli app sleep web >/dev/null 2>&1
wait_phase web asleep >/dev/null 2>&1 || bad "web did not sleep"
cs0="$(usage_field web compute_vcpu_seconds)"
sleep 6  # several ticks while asleep
cs1="$(usage_field web compute_vcpu_seconds)"
band "$(python3 -c "print(abs(float('$cs1')-float('$cs0')))")" 0 0.5 \
  && ok "compute frozen while asleep ($cs0 → $cs1)" || bad "compute moved while asleep ($cs0 → $cs1)"

echo "== 06 wake RESUMES accrual (compute + requests)"
cli app wake web >/dev/null 2>&1
wait_phase web running >/dev/null 2>&1 || bad "web did not wake"
cw0="$(usage_field web compute_vcpu_seconds)"
d6="$(hit web 5)"; web_reqs=$((web_reqs + d6))
sleep 6
cw1="$(usage_field web compute_vcpu_seconds)"; rw2="$(usage_field web requests)"
band "$(python3 -c "print(float('$cw1')-float('$cw0'))")" 4 24 \
  && ok "compute resumed after wake ($cw0 → $cw1)" || bad "compute did not resume after wake ($cw0 → $cw1)"
[[ "$rw2" == "$web_reqs" ]] && ok "requests continued counting across sleep/wake (now $web_reqs)" || bad "web requests = $rw2 (want $web_reqs after wake)"

echo "== 07 STORAGE accrues — and keeps accruing while ASLEEP"
cli volume create dbvol --size 512M >/dev/null 2>&1 || { bad "volume create failed"; tail -15 "$DAEMON_LOG"; }
cli app create db --image "$IMAGE" --pull missing --port 80 --restart always --health "http:80:/" \
  --vcpus 1 --memory 128 --volume dbvol:/data >/dev/null 2>&1
if wait_phase db running; then ok "volume app 'db' running"; else bad "db never ran"; tail -20 "$DAEMON_LOG"; fi
sleep 6
st0="$(usage_field db storage_gib_seconds)"
band "$st0" 0.0000001 100000 && ok "storage accrues while awake (${st0} GiB·s)" || bad "storage = ${st0} (want > 0)"
cli app sleep db >/dev/null 2>&1
wait_phase db asleep >/dev/null 2>&1 || bad "db did not sleep"
ss0="$(usage_field db storage_gib_seconds)"; dc0="$(usage_field db compute_vcpu_seconds)"
sleep 6
ss1="$(usage_field db storage_gib_seconds)"; dc1="$(usage_field db compute_vcpu_seconds)"
python3 -c "import sys; sys.exit(0 if float('$ss1')>float('$ss0') else 1)" \
  && ok "storage kept accruing while asleep (${ss0} → ${ss1} GiB·s)" || bad "storage frozen while asleep (${ss0} → ${ss1})"
band "$(python3 -c "print(abs(float('$dc1')-float('$dc0')))")" 0 0.5 \
  && ok "…while the slept app's compute stayed frozen" || bad "db compute moved while asleep ($dc0 → $dc1)"

echo "== 08 a DELETED app's final usage is retained"
cli app rm api >/dev/null 2>&1
sleep 1
api_state="$(cli app usage -o json 2>/dev/null | python3 -c '
import json,sys
for u in json.load(sys.stdin):
    if u.get("app_name")=="api":
        print("deleted" if u.get("finalized_at") else "live", u.get("requests")); break
else: print("gone")')"
case "$api_state" in
  "deleted 12") ok "deleted app 'api' retained with finalized usage (requests=12)" ;;
  deleted*)     ok "deleted app 'api' retained (finalized): $api_state" ;;
  *)            bad "deleted app usage not retained (got '$api_state')" ;;
esac

echo "== 09 usage SURVIVES a daemon restart (downtime not back-filled)"
rw_before="$(usage_field web requests)"; cw_before="$(usage_field web compute_vcpu_seconds)"
cli app sleep web >/dev/null 2>&1; wait_phase web asleep >/dev/null 2>&1 || true
c_slept="$(usage_field web compute_vcpu_seconds)"
stop_daemon
sleep 4  # downtime — must NOT be back-filled into compute
start_daemon || exit 3
wait_phase web asleep 60 >/dev/null 2>&1 || true
rw_after="$(usage_field web requests)"; cw_after="$(usage_field web compute_vcpu_seconds)"
[[ "$rw_after" == "$rw_before" ]] && ok "requests durable across restart ($rw_before → $rw_after)" \
  || bad "requests changed across restart ($rw_before → $rw_after)"
band "$(python3 -c "print(abs(float('$cw_after')-float('$c_slept')))")" 0 0.5 \
  && ok "compute durable + downtime not back-filled ($c_slept → $cw_after)" \
  || bad "compute changed across restart ($c_slept → $cw_after)"

cli app rm web >/dev/null 2>&1 || true; cli app rm db >/dev/null 2>&1 || true

echo "==============================================================="
echo " usage smoke: $pass passed, $fail failed"
echo " transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$fail" -eq 0 ]]
