#!/usr/bin/env bash
#
# bench_masswake.sh — the "50 DBs wake at once" load test.
#
# Scale-to-zero's economic bet is packing more sleeping apps than host RAM, which
# makes "everything wakes at the same time" a designed-for scenario, not an
# outage. This bench proves it before the first invoice: it boots N scale-to-zero
# apps behind the ingress proxy, drains the whole fleet with `app sleep --all`,
# then fires N concurrent wakes and measures what happens.
#
# What it reports (it is a measurement, not a pass/fail smoke):
#   * wake-latency distribution across the herd (p50 / p90 / p99 / max), both the
#     daemon-measured restore (last_wake_latency_ms) and the client-observed
#     end-to-end first-byte time;
#   * how many wakes were ADMITTED vs refused by the RAM floor (--wake-min-free-mib):
#     a refused wake is a clean 503, and the app stays asleep and retryable — the
#     graceful-degradation property. Refused wakes are retried sequentially and
#     must all eventually serve;
#   * the host MemAvailable low-water mark during the herd, and snapshot_disk_bytes.
#
# The only hard failure is an app that never serves even after a sequential retry.
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# curl, python3, a default egress iface, and internet (pulls nginx:alpine) or a
# cached image. Uses a /var/lib work root (NOT /tmp) so snapshot memory files hit
# real disk and the free-RAM measurement is honest.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        N=20 MEM=256 scripts/bench_masswake.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7914}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-bench-masswake}"
IMAGE="${IMAGE:-nginx:alpine}"
DOMAIN="${DOMAIN:-bench.local}"
PROXY_PORT="${PROXY_PORT:-8099}"
N="${N:-20}"                 # number of scale-to-zero apps in the fleet
MEM="${MEM:-256}"            # MiB per app
WAKE_MIN_FREE="${WAKE_MIN_FREE:-256}"  # --wake-min-free-mib (the admission floor under test)

echo "==============================================================="
echo " crucible mass-wake load test (N=$N apps, ${MEM} MiB each)"
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

echo "== prepare work root ($MOUNT), egress $EGRESS"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"; RESULT_DIR="$MOUNT/results"; mkdir -p "$RESULT_DIR"

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
  --app-db "$MOUNT/apps.db" --network-egress-iface "$EGRESS" \
  --proxy-listen ":$PROXY_PORT" --proxy-domain "$DOMAIN" \
  --wake-min-free-mib "$WAKE_MIN_FREE" \
  --log-format json --log-level warn >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
echo "   daemon healthy (pid $DAEMON_PID), wake floor ${WAKE_MIN_FREE} MiB"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_phase() { local want="$2" tries="${3:-400}"; for _ in $(seq 1 "$tries"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.1; done; return 1; }
wake_ms() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"last_wake_latency_ms":[0-9]*' | grep -o '[0-9]*'; }
fc_count() { pgrep -f 'firecracker --id' 2>/dev/null | wc -l | tr -d ' '; }
mem_avail() { awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo; }
metric() { curl -s "$BASE_URL/metrics" 2>/dev/null | awk -v m="$1" '$1==m {print $2; exit}'; }
# hit APP → curl the app through the proxy, print "<http_code> <e2e_ms>".
hit() {
  # max-time generous: under a 20-wide restore herd a single wake can queue behind
  # the others, and a client timeout here would look like a failure rather than
  # slow-but-served.
  curl -s -o /dev/null -H "Host: $1.$DOMAIN" -w '%{http_code} %{time_starttransfer}' \
    --max-time 90 "http://127.0.0.1:$PROXY_PORT/" 2>/dev/null || echo "000 0"
}

echo "== 00 pre-warm the image (one conversion, cached — avoids N concurrent first-converts)"
WARM="$(run run "$IMAGE" --pull missing -- true 2>/dev/null)"
[[ -n "$WARM" ]] && run rm "$WARM" >/dev/null 2>&1 || true
echo "   image ready, MemAvailable $(mem_avail) MiB"

echo "== 01 boot $N scale-to-zero apps, ONE at a time (setup: no boot storm)"
# Create+wait each app sequentially so only one VM boots at a time — the reconciler
# would otherwise try to boot all N at once (a RAM + convert storm). The HERD in
# step 03 is what's concurrent; setup need not be. Bounded per-app wait so a
# straggler can't hang the run, with visible progress.
ORIG_N="$N"
APPS=()
for i in $(seq 1 "$N"); do
  run app create "app$i" --image "$IMAGE" --pull missing --port 80 --restart always \
    --health "http:80:/" --memory "$MEM" --min-scale 0 >/dev/null 2>&1
  if wait_phase "app$i" running 300; then
    APPS+=("app$i")
  else
    echo "   app$i failed to reach running (phase=$(app_phase app$i)); skipping"
  fi
  printf "\r   booted %d/%d (MemAvailable %d MiB)   " "${#APPS[@]}" "$i" "$(mem_avail)"
done
echo
N="${#APPS[@]}"   # measure the herd over the apps that actually booted
[[ "$N" -ge 2 ]] || { echo "   too few apps booted ($N) to measure a herd; check $DAEMON_LOG"; tail -20 "$DAEMON_LOG"; exit 1; }

echo "== 02 drain the fleet: app sleep --all"
run app sleep --all >/dev/null 2>&1
SLEPT=0; for a in "${APPS[@]}"; do [[ "$(app_phase "$a")" == "asleep" ]] && SLEPT=$((SLEPT+1)); done
wait_phase "${APPS[0]}" asleep 60 || echo "   warn: ${APPS[0]} not asleep"
for _ in $(seq 1 60); do [[ "$(fc_count)" -eq 0 ]] && break; sleep 0.5; done
echo "   $SLEPT/$N asleep, $(fc_count) VMs running, MemAvailable $(mem_avail) MiB, snapshot_disk_bytes $(metric snapshot_disk_bytes)"

echo "== 03 the herd: $N concurrent wakes"
# Sample MemAvailable low-water in the background while the herd wakes (runs until
# killed, so it never gates the step).
LOWWATER_FILE="$RESULT_DIR/lowwater"; echo "$(mem_avail)" >"$LOWWATER_FILE"
( while :; do
    cur=$(mem_avail); lo=$(cat "$LOWWATER_FILE" 2>/dev/null || echo "$cur")
    (( cur < lo )) && echo "$cur" >"$LOWWATER_FILE"
    sleep 0.1
  done ) & SAMPLER=$!
# Fire all wakes at once; each records "<code> <e2e_ms>" to its own file. Wait on
# the wake jobs specifically (not the sampler), so the step ends when the herd is
# served, then stop the sampler.
HERD_PIDS=()
for a in "${APPS[@]}"; do hit "$a" >"$RESULT_DIR/$a" & HERD_PIDS+=("$!"); done
wait "${HERD_PIDS[@]}"
kill "$SAMPLER" 2>/dev/null || true

echo "== 04 aggregate + retry any RAM-floor refusals"
SERVED=0; REFUSED=0; FAILED=0
E2E=(); WAKES=()
for a in "${APPS[@]}"; do
  read -r code e2e <"$RESULT_DIR/$a"
  case "$code" in
    2*) SERVED=$((SERVED+1)); E2E+=("$(python3 -c "print(int(float('$e2e')*1000))")");;
    503) REFUSED=$((REFUSED+1));;
    *)  FAILED=$((FAILED+1)); echo "   $a: unexpected code $code";;
  esac
done
# Refused wakes (503, admission floor) must eventually serve on a sequential retry.
RETRY_OK=0
if [[ "$REFUSED" -gt 0 ]]; then
  echo "   $REFUSED wake(s) hit the RAM floor (clean 503); retrying sequentially..."
  for a in "${APPS[@]}"; do
    read -r code _ <"$RESULT_DIR/$a"
    [[ "$code" == "503" ]] || continue
    ok=0
    for _ in $(seq 1 60); do
      r="$(hit "$a")"; [[ "${r%% *}" == 2* ]] && { ok=1; break; }
      sleep 1
    done
    [[ "$ok" -eq 1 ]] && RETRY_OK=$((RETRY_OK+1)) || { FAILED=$((FAILED+1)); echo "   $a never served even after retry"; }
  done
fi

# wake latencies (daemon-measured) across whatever is now awake
for a in "${APPS[@]}"; do w="$(wake_ms "$a")"; [[ -n "$w" ]] && WAKES+=("$w"); done

pct() { # pct P v1 v2 …  → the P-th percentile (nearest-rank)
  local p="$1"; shift; [[ $# -eq 0 ]] && { echo "-"; return; }
  printf '%s\n' "$@" | sort -n | awk -v p="$p" -v n="$#" 'BEGIN{r=int((p/100)*n+0.5); if(r<1)r=1} NR==r{print; exit}'
}
echo "==============================================================="
echo " RESULTS (N=$N, ${MEM} MiB/app, wake floor ${WAKE_MIN_FREE} MiB)"
echo "  admitted first-shot : $SERVED    floor-refused (503): $REFUSED    retried-served: $RETRY_OK    FAILED: $FAILED"
echo "  MemAvailable lowwater: $(cat "$LOWWATER_FILE") MiB    snapshot_disk_bytes: $(metric snapshot_disk_bytes)"
if [[ "${#E2E[@]}" -gt 0 ]]; then
  echo "  client e2e first-byte : p50 $(pct 50 "${E2E[@]}")  p90 $(pct 90 "${E2E[@]}")  p99 $(pct 99 "${E2E[@]}")  max $(pct 100 "${E2E[@]}") ms"
fi
if [[ "${#WAKES[@]}" -gt 0 ]]; then
  echo "  daemon wake latency   : p50 $(pct 50 "${WAKES[@]}")  p90 $(pct 90 "${WAKES[@]}")  p99 $(pct 99 "${WAKES[@]}")  max $(pct 100 "${WAKES[@]}") ms"
fi
echo "==============================================================="
for i in $(seq 1 "$ORIG_N"); do run app rm "app$i" >/dev/null 2>&1; done
[[ "$FAILED" -eq 0 ]]
