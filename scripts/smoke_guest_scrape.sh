#!/usr/bin/env bash
#
# smoke_guest_scrape.sh — guest metrics scrape (v0.9.0).
#
# Proves the daemon scrapes a Prometheus endpoint inside a guest and folds the
# series into its own /metrics, and that scraping respects scale-to-zero:
#
#   03  an app exposes a Prometheus /metrics inside the guest (here nginx serves a
#       faux postgres_exporter exposition, so the smoke needs no real exporter).
#   04  the scraped series appear on the daemon /metrics with app+instance labels,
#       and crucible_guest_scrape_up{app=web} == 1.
#   05  when the app SLEEPS, crucible_guest_scrape_up goes to 0, the scraped series
#       drop out, and — critically — scraping does NOT wake it (it stays asleep).
#   06  waking the app brings the scrape (and scrape_up=1) back.
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# curl, a pullable nginx:alpine, a default egress iface.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_guest_scrape.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7930}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-scrape}"
IMAGE="${IMAGE:-nginx:alpine}"
PORT="${PORT:-18095}"
INTERVAL="${INTERVAL:-2s}"      # short so the smoke doesn't wait a full 15s

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible guest metrics scrape smoke (v0.9.0)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>)" >&2; exit 2; }

echo "== 01 prepare work root ($MOUNT)"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"

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
  --guest-scrape-interval "$INTERVAL" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }
grep -q "guest metrics scrape enabled" "$DAEMON_LOG" && ok "daemon up, guest scrape enabled" || bad "scrape not enabled"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
phase_of() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_ph() { for _ in $(seq 1 300); do [[ "$(phase_of "$1")" == "$2" ]] && return 0; sleep 0.3; done; return 1; }
scrape_up() { curl -s "$BASE_URL/metrics" 2>/dev/null | awk '/^crucible_guest_scrape_up\{.*app="web"/ {print $2; exit}'; }
has_series() { curl -s "$BASE_URL/metrics" 2>/dev/null | grep -q "pg_stat_database_blks_hit{.*app=\"web\".*} 123456"; }

echo "== 02 create a scale-to-zero app that exposes /metrics on :80"
cerr="$(run app create web --image "$IMAGE" -p "$PORT":80 --min-scale 0 --idle-timeout 1h \
  --metrics-port 80 --metrics-path /metrics --vcpus 1 --memory 256 2>&1)" \
  || { bad "app create failed: $cerr"; tail -20 "$DAEMON_LOG"; exit 1; }
wait_ph web running && ok "app running" || { bad "app never ran"; run app get web 2>&1 | head -c 300; tail -20 "$DAEMON_LOG"; exit 1; }

echo "== 03 serve a faux postgres_exporter exposition from the guest"
run app exec web -- sh -c 'printf "# HELP pg_stat_database_blks_hit Buffer cache hits\n# TYPE pg_stat_database_blks_hit counter\npg_stat_database_blks_hit{datname=\"app\"} 123456\n# HELP pg_up up\n# TYPE pg_up gauge\npg_up 1\n" > /usr/share/nginx/html/metrics' >/dev/null 2>&1 \
  && ok "wrote /metrics inside the guest" || bad "could not write guest /metrics"

echo "== 04 scraped series appear on the daemon /metrics with app+instance labels"
for _ in $(seq 1 20); do has_series && break; sleep 0.5; done
has_series && ok "pg_stat_database_blks_hit{app=web,...} = 123456 on /metrics" || { bad "scraped series not found"; curl -s "$BASE_URL/metrics" | grep -i 'pg_\|guest_scrape' | head; }
[[ "$(scrape_up)" == "1" ]] && ok "crucible_guest_scrape_up{app=web} = 1" || bad "scrape_up != 1 ($(scrape_up))"

echo "== 05 sleep: scrape_up→0, series drop, and scraping does NOT wake it"
run app sleep web >/dev/null 2>&1
wait_ph web asleep && ok "app asleep" || bad "app did not sleep (phase=$(phase_of web))"
sleep 4   # let a scrape cycle (interval 2s) observe the slept instance
[[ "$(scrape_up)" == "0" ]] && ok "crucible_guest_scrape_up{app=web} = 0 while asleep" || bad "scrape_up != 0 while asleep ($(scrape_up))"
has_series && bad "SECURITY/BUG: scraped series still present while asleep" || ok "scraped series dropped while asleep"
# The scrape must not have woken it — after several scrape cycles it is still asleep.
[[ "$(phase_of web)" == "asleep" ]] && ok "scraping did NOT wake the slept app" || bad "app was woken (phase=$(phase_of web))"

echo "== 06 wake: scrape resumes"
run app wake web >/dev/null 2>&1
wait_ph web running && ok "app woke" || bad "app did not wake"
for _ in $(seq 1 20); do [[ "$(scrape_up)" == "1" ]] && break; sleep 0.5; done
[[ "$(scrape_up)" == "1" ]] && ok "scrape_up back to 1 after wake" || bad "scrape_up != 1 after wake ($(scrape_up))"

run app rm web >/dev/null 2>&1 || true

echo "==============================================================="
echo " guest scrape smoke: $pass passed, $fail failed"
echo " transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$fail" -eq 0 ]]
