#!/usr/bin/env bash
#
# bench_encryption.sh — what do the at-rest features cost?
#
# Measures the runtime overhead of the security/persistence features so the
# "it's basically free" claims are numbers, not adjectives. It A/Bs the features
# that HAVE a toggle to compare against:
#
#   A  Volume format       — `volume create` plaintext vs. --encrypt (one-time
#                            luksFormat cost).
#   B  Snapshot wake        — sleep→wake latency (daemon-measured + client e2e) for
#                            a volume app, plaintext vs. encrypted volume. The
#                            encrypted delta is one luksOpen (fast-KDF keyslot).
#   C  Guest volume I/O     — sequential write throughput through the mounted
#                            volume, plaintext vs. encrypted (the dm-crypt AES-NI
#                            cost, symmetric with reads).
#   D  Secret injection     — boot-to-running for an app with no secrets vs. one
#                            with an N-key bundle (envFrom decrypt + merge at boot).
#
# It is a MEASUREMENT, not a pass/fail smoke — it prints a comparison table.
# (The always-on accounting features — usage metrics v0.7.1, egress bytes v0.7.2 —
# have no runtime toggle to A/B; their cost is a per-packet nft counter increment
# and a periodic ledger tick, not something this harness can isolate.)
#
# The work root is a btrfs loopback so per-sandbox rootfs clones + snapshots use
# reflinks (O(1)) — otherwise byte-copies inflate the absolute wake numbers on an
# ext4 host. WORK_ENCRYPT=1 puts that btrfs on dm-crypt to also measure an
# encrypted --work-base (and confirm the startup advisory goes quiet).
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# cryptsetup, mkfs.btrfs (btrfs-progs), findmnt, curl, python3, a pullable
# nginx:alpine. Runs under /var/lib (NOT /tmp) so I/O + snapshots hit real disk.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/bench_encryption.sh
#   # …and again with WORK_ENCRYPT=1 to measure an encrypted --work-base.

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7926}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-bench-enc}"
IMAGE="${IMAGE:-nginx:alpine}"
R_WAKE="${R_WAKE:-5}"          # sleep/wake cycles measured per volume type
R_FMT="${R_FMT:-3}"            # volume-format samples per type
IO_MIB="${IO_MIB:-256}"       # guest write-throughput test size
SECRET_KEYS="${SECRET_KEYS:-25}"  # keys in the secret bundle for section D
MEM="${MEM:-384}"
VOL_SIZE="${VOL_SIZE:-512M}"
PORT_BASE="${PORT_BASE:-18080}"   # wake-bench apps publish PORT_BASE+1 / +2 (scale-to-zero needs a wake trigger)
# The work root is a btrfs loopback so per-sandbox rootfs clones + snapshots use
# reflinks (O(1)) — honest wake numbers, not byte-copy-inflated. WORK_ENCRYPT=1
# puts that btrfs on a dm-crypt layer, so --work-base (snapshot memory + rootfs) is
# itself encrypted at rest: the recommended production setup, and it lets us
# measure its cost + confirm the startup advisory goes silent. Run it both ways.
WORK_ENCRYPT="${WORK_ENCRYPT:-0}"
IMG="${IMG:-${MOUNT}.img}"
IMG_SIZE="${IMG_SIZE:-16G}"
WORK_KEYFILE="${MOUNT}.workkey"
WORKMAP="benchwork"

echo "==============================================================="
echo " crucible at-rest overhead bench (encryption + secrets)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer + cryptsetup)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" && -r "$ROOTFS" && -r /dev/kvm ]] || { echo "error: kernel/rootfs/kvm not readable" >&2; exit 2; }
command -v cryptsetup >/dev/null || { echo "error: cryptsetup needed" >&2; exit 2; }
command -v mkfs.btrfs >/dev/null || { echo "error: mkfs.btrfs needed (btrfs-progs) for the reflink work root" >&2; exit 2; }
command -v findmnt    >/dev/null || { echo "error: findmnt needed (util-linux)" >&2; exit 2; }
command -v python3    >/dev/null || { echo "error: python3 needed" >&2; exit 2; }
command -v curl       >/dev/null || { echo "error: curl needed" >&2; exit 2; }
systemctl is-active --quiet crucible 2>/dev/null && { echo "error: stop the systemd crucible first" >&2; exit 2; }
# The wake-bench apps publish a host port (scale-to-zero needs a wake trigger),
# which requires the network provisioner — so the daemon needs an egress iface.
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>)" >&2; exit 2; }

# ---- work root: btrfs loopback (reflink), optionally on dm-crypt ------------
echo "== 00 work root: btrfs loopback$( [[ "$WORK_ENCRYPT" == 1 ]] && echo ' on dm-crypt (encrypted --work-base)' )"
umount "$MOUNT" 2>/dev/null || true
[[ -e "/dev/mapper/$WORKMAP" ]] && cryptsetup close "$WORKMAP" 2>/dev/null || true
rm -rf "$MOUNT"; mkdir -p "$MOUNT"
truncate -s "$IMG_SIZE" "$IMG"
if [[ "$WORK_ENCRYPT" == 1 ]]; then
  head -c 32 /dev/urandom > "$WORK_KEYFILE"
  cryptsetup luksFormat --type luks2 --batch-mode --pbkdf pbkdf2 --pbkdf-force-iterations 1000 "$IMG" --key-file "$WORK_KEYFILE"
  cryptsetup open --type luks2 "$IMG" "$WORKMAP" --key-file "$WORK_KEYFILE"
  mkfs.btrfs -q -f "/dev/mapper/$WORKMAP"
  mount "/dev/mapper/$WORKMAP" "$MOUNT"
else
  mkfs.btrfs -q -f "$IMG"
  mount -o loop "$IMG" "$MOUNT"
fi
findmnt -no FSTYPE "$MOUNT" | grep -q btrfs || { echo "error: $MOUNT is not btrfs" >&2; exit 3; }
mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"   # staged on the target FS so clones reflink
DAEMON_LOG="$MOUNT/daemon.log"
echo "   $MOUNT = btrfs (reflink)$( [[ "$WORK_ENCRYPT" == 1 ]] && echo ' / dm-crypt' ), $(df -h --output=avail "$MOUNT" | tail -1 | tr -d ' ') free"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  # Close volume crypt devices (their containers are files on the work root) first,
  # then unmount the work root and close its own crypt layer.
  for m in /dev/mapper/crucible-vol-* /dev/mapper/crucible-fmt-*; do
    [[ -e "$m" ]] && cryptsetup close "$(basename "$m")" 2>/dev/null || true
  done
  if [[ "${KEEP:-0}" != "1" ]]; then
    sleep 1; umount "$MOUNT" 2>/dev/null || true
    [[ -e "/dev/mapper/$WORKMAP" ]] && cryptsetup close "$WORKMAP" 2>/dev/null || true
    rm -f "$IMG" "$WORK_KEYFILE"; rm -rf "$MOUNT"
  fi
}
trap cleanup EXIT

"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --volume-dir "$MOUNT/volumes" --volume-encrypt-key-file "$MOUNT/volume.key" \
  --secrets-key-file "$MOUNT/secrets.key" --network-egress-iface "$EGRESS" \
  --app-db "$MOUNT/apps.db" --log-format json --log-level warn >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -20 "$DAEMON_LOG"; exit 3; }

# Validate the startup advisory (#1): it must fire on a plaintext work root and go
# silent on a dm-crypt one — the encryption-at-rest honesty check, end to end.
if grep -q "work-base is on unencrypted storage" "$DAEMON_LOG"; then FIRED=1; else FIRED=0; fi
if [[ "$WORK_ENCRYPT" == 1 ]]; then
  [[ "$FIRED" == 0 ]] && echo "   advisory: silent on encrypted --work-base ✓" || echo "   advisory: FIRED on an encrypted work-base ✗ (misdetection)"
else
  [[ "$FIRED" == 1 ]] && echo "   advisory: fired on plaintext --work-base ✓" || echo "   advisory: did NOT fire on a plaintext work-base ✗"
fi

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
# epoch-ms via nanoseconds ÷ 1e6 — robust when `date` ignores the %3N width.
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
wait_phase() { local want="$2" tries="${3:-400}"; for _ in $(seq 1 "$tries"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.3; done; return 1; }
wake_ms() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"last_wake_latency_ms":[0-9]*' | grep -o '[0-9]*'; }
mean()  { python3 -c "import sys; xs=[float(x) for x in sys.argv[1:]]; print(round(sum(xs)/len(xs),1) if xs else 0)" "$@"; }
pctl()  { python3 -c "import sys,math; p=float(sys.argv[1]); xs=sorted(float(x) for x in sys.argv[2:]); print(round(xs[max(0,math.ceil(p/100*len(xs))-1)],1) if xs else 0)" "$@"; }
ovh()   { python3 -c "import sys; a,b=float(sys.argv[1]),float(sys.argv[2]); print('%+.0f%%' % ((b-a)/a*100) if a else 'n/a')" "$1" "$2"; }

# ---- A: volume format cost --------------------------------------------------
echo "== A volume format: plaintext vs --encrypt ($R_FMT samples each)"
FMT_PLAIN=(); FMT_ENC=()
for i in $(seq 1 "$R_FMT"); do
  t0=$(now_ms); run volume create "fp$i" --no-encrypt --size "$VOL_SIZE" >/dev/null 2>&1; t1=$(now_ms); FMT_PLAIN+=($((t1-t0)))
  t0=$(now_ms); run volume create "fe$i" --encrypt    --size "$VOL_SIZE" >/dev/null 2>&1; t1=$(now_ms); FMT_ENC+=($((t1-t0)))
done
FMT_P=$(mean "${FMT_PLAIN[@]}"); FMT_E=$(mean "${FMT_ENC[@]}")
echo "   plaintext ${FMT_P}ms   encrypted ${FMT_E}ms   ($(ovh "$FMT_P" "$FMT_E"))"

# ---- build the two wake/IO apps (identical but for volume encryption) --------
echo "== setup: boot one app per volume type ($IMAGE)"
run volume create wplain --no-encrypt --size "$VOL_SIZE" >/dev/null 2>&1
run volume create wenc   --encrypt    --size "$VOL_SIZE" >/dev/null 2>&1
boot_app() { # name vol hostport — create + wait running, surfacing errors; first boot warms the pull
  local name="$1" vol="$2" port="$3" cerr
  # A scale-to-zero app (min-scale 0) needs a wake trigger; publish a host port so
  # it's valid. We still drive sleep/wake manually below — the forwarder sits idle.
  cerr="$(run app create "$name" --image "$IMAGE" --volume "$vol":/data -p "$port":80 --min-scale 0 --idle-timeout 1h --vcpus 1 --memory "$MEM" 2>&1)" \
    || { echo "app create $name failed: $cerr"; tail -20 "$DAEMON_LOG"; exit 1; }
  wait_phase "$name" running 400 || { echo "$name never reached running (phase=$(app_phase "$name"); image $IMAGE pullable?)"; run app get "$name" 2>&1 | head -c 400; echo; tail -20 "$DAEMON_LOG"; exit 1; }
}
boot_app benchplain wplain "$((PORT_BASE+1))"   # first app pays the pull; subsequent boots use the cache
boot_app benchenc   wenc   "$((PORT_BASE+2))"

# ---- B: snapshot wake latency ----------------------------------------------
echo "== B snapshot wake: plaintext vs encrypted volume ($R_WAKE cycles each)"
wake_cycle() { # app → prints "daemon_ms e2e_ms"
  local app="$1"
  run app sleep "$app" >/dev/null 2>&1; wait_phase "$app" asleep 300 || { echo "0 0"; return; }
  local t0 t1; t0=$(now_ms); run app wake "$app" >/dev/null 2>&1; wait_phase "$app" running 300; t1=$(now_ms)
  echo "$(wake_ms "$app") $((t1-t0))"
}
BD=(); BE=(); ED=(); EE=()
for _ in $(seq 1 "$R_WAKE"); do read -r d e <<<"$(wake_cycle benchplain)"; BD+=("$d"); BE+=("$e"); done
for _ in $(seq 1 "$R_WAKE"); do read -r d e <<<"$(wake_cycle benchenc)";   ED+=("$d"); EE+=("$e"); done
BD_M=$(mean "${BD[@]}"); ED_M=$(mean "${ED[@]}"); BE_M=$(mean "${BE[@]}"); EE_M=$(mean "${EE[@]}")
echo "   daemon restore  plaintext ${BD_M}ms (p90 $(pctl 90 "${BD[@]}"))   encrypted ${ED_M}ms (p90 $(pctl 90 "${ED[@]}"))   ($(ovh "$BD_M" "$ED_M"))"
echo "   client e2e      plaintext ${BE_M}ms (p90 $(pctl 90 "${BE[@]}"))   encrypted ${EE_M}ms (p90 $(pctl 90 "${EE[@]}"))   ($(ovh "$BE_M" "$EE_M"))"

# ---- C: guest volume write throughput --------------------------------------
echo "== C guest volume write throughput: plaintext vs encrypted (${IO_MIB} MiB)"
io_mbps() { # app → MB/s writing IO_MIB through the mounted volume, flushed
  local app="$1" t0 t1
  wait_phase "$app" running 300
  t0=$(now_ms)
  run app exec "$app" -- sh -c "dd if=/dev/zero of=/data/bench bs=1M count=$IO_MIB conv=fdatasync 2>/dev/null; rm -f /data/bench" >/dev/null 2>&1
  t1=$(now_ms)
  python3 -c "print(round($IO_MIB*1000/max(1,$t1-$t0),1))"
}
IO_P=$(io_mbps benchplain); IO_E=$(io_mbps benchenc)
echo "   plaintext ${IO_P} MB/s   encrypted ${IO_E} MB/s   ($(ovh "$IO_P" "$IO_E"))"
echo "   note: this write path is host-side Writeback-cached, so it reflects guest→host"
echo "   bandwidth more than disk — the clean cipher cost is C2 below."

# ---- C2: raw dm-crypt cipher throughput (host, cryptsetup benchmark) --------
# The honest encryption-layer ceiling on THIS cpu, independent of the VM's
# writeback cache: aes-xts-512 (= AES-256-XTS, what an encrypted volume uses).
echo "== C2 dm-crypt cipher throughput on this host (cryptsetup benchmark, aes-xts-512)"
CB=$(cryptsetup benchmark --cipher aes-xts --key-size 512 2>/dev/null | awk '/aes-xts/ && /512b/ {print $3" "$4" "$5" "$6}')
[[ -n "$CB" ]] && echo "   aes-xts 512b: $CB" || echo "   (cryptsetup benchmark unavailable)"

# ---- D: secret injection at boot -------------------------------------------
echo "== D secret injection: no secrets vs an ${SECRET_KEYS}-key bundle (boot-to-running)"
ENVFILE="$MOUNT/bench.env"; : >"$ENVFILE"
for i in $(seq 1 "$SECRET_KEYS"); do echo "BENCH_KEY_$i=value-$i-abcdefghijklmnop" >>"$ENVFILE"; done
run secret set benchbundle --from-env-file "$ENVFILE" >/dev/null 2>&1
boot_ms() { # extra-args… → create→running ms for a fresh app
  local name="$1"; shift
  local t0 t1; t0=$(now_ms)
  run app create "$name" --image "$IMAGE" --min-scale 1 --vcpus 1 --memory "$MEM" "$@" >/dev/null 2>&1
  wait_phase "$name" running 600; t1=$(now_ms)
  echo $((t1-t0))
}
BOOT_NONE=$(boot_ms bootnos); BOOT_SEC=$(boot_ms bootsec --secrets benchbundle)
echo "   no secrets ${BOOT_NONE}ms   ${SECRET_KEYS}-key bundle ${BOOT_SEC}ms   ($(ovh "$BOOT_NONE" "$BOOT_SEC"))"

# ---- summary ----------------------------------------------------------------
echo "==============================================================="
echo " summary (plaintext/baseline  →  encrypted/with-feature  = overhead)"
echo "==============================================================="
printf "  %-26s %10s  %10s  %8s\n" "metric" "baseline" "feature" "overhead"
printf "  %-26s %10s  %10s  %8s\n" "volume format (ms)"        "$FMT_P" "$FMT_E" "$(ovh "$FMT_P" "$FMT_E")"
printf "  %-26s %10s  %10s  %8s\n" "wake, daemon (ms)"         "$BD_M"  "$ED_M"  "$(ovh "$BD_M" "$ED_M")"
printf "  %-26s %10s  %10s  %8s\n" "wake, e2e (ms)"            "$BE_M"  "$EE_M"  "$(ovh "$BE_M" "$EE_M")"
printf "  %-26s %10s  %10s  %8s\n" "volume write (MB/s)"       "$IO_P"  "$IO_E"  "$(ovh "$IO_P" "$IO_E")"
printf "  %-26s %10s  %10s  %8s\n" "boot w/ secrets (ms)"      "$BOOT_NONE" "$BOOT_SEC" "$(ovh "$BOOT_NONE" "$BOOT_SEC")"
printf "  %-26s %s\n" "dm-crypt aes-xts-512" "${CB:-n/a}  (enc / dec ceiling)"
echo "  (negative overhead = feature is faster, i.e. within run-to-run noise."
echo "   The honest cipher cost is the aes-xts-512 line: it dwarfs real I/O rates,"
echo "   so dm-crypt is not the bottleneck. The 'volume write' row is Writeback-"
echo "   cached (guest→host bandwidth), not disk. Wake pays a fixed one-time"
echo "   luksOpen (tens of ms); format pays a one-time luksFormat; secret injection"
echo "   is lost in boot noise. A reflink --work-base (btrfs/XFS) lowers wake on"
echo "   both sides.)"
echo "  transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
