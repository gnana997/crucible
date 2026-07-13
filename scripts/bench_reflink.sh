#!/usr/bin/env bash
#
# One-command benchmark run for crucible-bench, on either filesystem.
#
#   FS=btrfs (default) — creates a btrfs (reflink) loopback and stages the rootfs
#                        + work dirs on it, so snapshot/fork/wake reflink.
#   FS=ext4            — uses a plain dir on the host's ext4 root (no loopback),
#                        so those same ops full byte-copy — the "common default".
#
# Run it twice (FS=btrfs then FS=ext4) to get both columns for docs/benchmarks.md.
# On this host / is ext4; the btrfs numbers are the ones worth publishing, the
# ext4 ones show the cost of NOT having reflink.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, mkfs.btrfs (btrfs mode),
# curl, python3, `make build && make bench` already run, and internet for
# nginx:alpine (the proxywake app) unless cached.
#
# Usage:
#   make build && make bench
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        FS=btrfs scripts/bench_reflink.sh
#   sudo ... FS=ext4 scripts/bench_reflink.sh
#
# Env knobs: FS, MOUNT, IMG, IMG_SIZE, DENSITY, SAMPLES, PHASES, OUT, KEEP=1.

set -u
set -o pipefail

FS="${FS:-btrfs}"
CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
BENCH_BIN="${BENCH_BIN:-./bin/crucible-bench}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
IMG="${IMG:-/var/lib/crucible-bench.img}"
IMG_SIZE="${IMG_SIZE:-60G}"
LISTEN="${LISTEN:-127.0.0.1:7878}"
PROXY_PORT="${PROXY_PORT:-7879}"
DOMAIN="${DOMAIN:-apps.local}"
SAMPLES="${SAMPLES:-30}"
BASE_URL="http://${LISTEN}"

case "$FS" in
  btrfs)
    MOUNT="${MOUNT:-/mnt/crucible-bench}"
    # density is RAM-bound on reflink → fine to push to 512.
    DENSITY="${DENSITY:-512}"
    PHASES="${PHASES:-latency,fanout,memory,density,proxywake}"
    ;;
  ext4)
    # A plain dir on the host root (ext4). density is DISK-bound on ext4 (each
    # fork byte-copies ~1 GiB rootfs), so it is off by default here.
    MOUNT="${MOUNT:-/var/lib/crucible-bench-ext4}"
    DENSITY="${DENSITY:-0}"
    PHASES="${PHASES:-latency,fanout,memory,proxywake}"
    ;;
  *) echo "error: FS must be btrfs or ext4 (got $FS)" >&2; exit 2 ;;
esac
OUT="${OUT:-bench-${FS}-$(date +%Y%m%d-%H%M%S).json}"

echo "==============================================================="
echo " crucible benchmark — FS=$FS   work root: $MOUNT   phases: $PHASES"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
[[ -x "$BENCH_BIN" ]]    || { echo "error: $BENCH_BIN not executable (make bench)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r "$ROOTFS" ]] || { echo "error: rootfs not readable: $ROOTFS" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null && command -v python3 >/dev/null || { echo "error: curl + python3 needed" >&2; exit 2; }
[[ "$FS" == btrfs ]] && { command -v mkfs.btrfs >/dev/null || { echo "error: mkfs.btrfs needed" >&2; exit 2; }; }
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

# ---- work root --------------------------------------------------------------
echo "== 01 prepare work root ($FS)"
if [[ "$FS" == btrfs ]]; then
  umount "$MOUNT" 2>/dev/null || true
  truncate -s "$IMG_SIZE" "$IMG"
  mkfs.btrfs -q -f "$IMG"
  mkdir -p "$MOUNT"
  mount -o loop "$IMG" "$MOUNT"
  findmnt -no FSTYPE "$MOUNT" | grep -q btrfs || { echo "error: $MOUNT is not btrfs" >&2; exit 3; }
else
  rm -rf "$MOUNT"; mkdir -p "$MOUNT"
fi
echo "   $MOUNT is $(findmnt -no FSTYPE "$MOUNT" 2>/dev/null || stat -f -c %T "$MOUNT") ($(df -h --output=avail "$MOUNT" | tail -1 | tr -d ' ') free)"

WORK="$MOUNT/run"; CHROOT="$MOUNT/jailer"; IMAGES="$MOUNT/images"; LOGS="$MOUNT/logs"; VOLUMES="$MOUNT/volumes"
mkdir -p "$WORK" "$CHROOT" "$IMAGES" "$LOGS" "$VOLUMES"
cp "$ROOTFS" "$MOUNT/rootfs.ext4"   # staged on the target FS so clones behave per-FS
DAEMON_LOG="$MOUNT/daemon.log"

# ---- daemon -----------------------------------------------------------------
DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id sbx-' 2>/dev/null || true
  if [[ "${KEEP:-0}" != "1" ]]; then
    if [[ "$FS" == btrfs ]]; then sleep 1; umount "$MOUNT" 2>/dev/null || true; rm -f "$IMG"; else rm -rf "$MOUNT"; fi
    echo "   (cleaned up $MOUNT; set KEEP=1 to keep)"
  fi
}
trap cleanup EXIT

echo "== 02 start daemon (--work-base $WORK)"
CRUCIBLE_MAX_FORK="${CRUCIBLE_MAX_FORK:-$((DENSITY > 0 ? DENSITY + 16 : 144))}" "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$CHROOT" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$WORK" --image-dir "$IMAGES" --log-dir "$LOGS" \
  --volume-dir "$VOLUMES" \
  --app-db "$MOUNT/apps.db" --network-egress-iface "$EGRESS_IFACE" \
  --proxy-listen "127.0.0.1:$PROXY_PORT" --proxy-domain "$DOMAIN" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do
  curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break
  kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
  sleep 0.2
done
echo "   daemon healthy (pid $DAEMON_PID)"

# ---- bench ------------------------------------------------------------------
echo "== 03 run crucible-bench"
"$BENCH_BIN" --addr "$LISTEN" \
  --reflink-path "$WORK" \
  --phases "$PHASES" --samples "$SAMPLES" --density "$DENSITY" \
  --proxy-addr "127.0.0.1:$PROXY_PORT" --proxy-domain "$DOMAIN" --wake-image nginx:alpine \
  --json "$OUT"

echo "==============================================================="
echo " done (FS=$FS). results: $OUT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
