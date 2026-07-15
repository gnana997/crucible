#!/usr/bin/env bash
#
# smoke_events.sh — app lifecycle events (v0.7.3).
#
# Drives a real app through its lifecycle and asserts the event stream carries
# each transition, in order, with a monotonic cursor and a working ?since resume:
#
#   01  daemon up (apps enabled)
#   02  create an app → a `created` event appears
#   03  the reconciler booting it → a `phase_changed` to running (from the sweep)
#   04  sleep → `phase_changed` to asleep; wake → `phase_changed` to running
#       carrying wake_latency_ms (distinguishes a wake from a boot)
#   05  update → an `updated` event
#   06  cursors are strictly monotonic across the whole stream
#   07  ?since=<cursor> replays ONLY newer events (delete after the cursor shows
#       up; the earlier `created` does not)
#
# Reads events via GET /events?app=<name>[&since=<seq>] (the daemon is loopback +
# unauthenticated in this smoke, so curl reaches it directly).
#
# Requires: root + KVM, firecracker + jailer + vmlinux + rootfs, crucible built,
# curl, python3, a pullable nginx:alpine.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_events.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7919}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-events}"
IMAGE="${IMAGE:-nginx:alpine}"

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible app lifecycle events smoke (v0.7.3)"
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

echo "== 01 start daemon (apps enabled)"
"$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
  --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
  --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
  --app-db "$MOUNT/apps.db" --network-egress-iface "$EGRESS" \
  --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
for _ in {1..150}; do curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 || { echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3; }
ok "daemon up"

cli() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }
app_phase() { cli app get "$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
wait_phase() { local want="$2" tries="${3:-240}"; for _ in $(seq 1 "$tries"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.5; done; return 1; }
events_json() { curl -s "$BASE_URL/events?app=web${1:+&since=$1}"; }
# has_event TYPE [ATTR VALUE] — 0 if an event of TYPE (optionally with Attrs[ATTR]==VALUE) exists.
has_event() {
  events_json | python3 -c '
import json,sys
typ=sys.argv[1]; ak=sys.argv[2] if len(sys.argv)>2 else None; av=sys.argv[3] if len(sys.argv)>3 else None
for e in json.load(sys.stdin).get("events",[]):
    if e.get("type")!=typ: continue
    if ak is None: sys.exit(0)
    if str((e.get("attrs") or {}).get(ak))==av: sys.exit(0)
sys.exit(1)' "$@"
}
# has_wake — 0 if a phase_changed to running carries a wake_latency_ms (a real wake).
has_wake() {
  events_json | python3 -c '
import json,sys
for e in json.load(sys.stdin).get("events",[]):
    a=e.get("attrs") or {}
    if e.get("type")=="phase_changed" and a.get("to")=="running" and "wake_latency_ms" in a: sys.exit(0)
sys.exit(1)'
}
cursor() { events_json | python3 -c 'import json,sys; print(json.load(sys.stdin).get("cursor",0))'; }

echo "== 02 create an app → a 'created' event"
cli app create web --image "$IMAGE" --pull missing --port 80 --restart always --health "http:80:/" --vcpus 1 --memory 256 >/dev/null 2>&1
if wait_phase web running; then ok "app 'web' running"; else bad "app never ran (is $IMAGE pullable?)"; tail -20 "$DAEMON_LOG"; exit 1; fi
has_event created && ok "'created' event emitted" || bad "no 'created' event"

echo "== 03 booting emits a phase_changed to running"
# the reconcile sweep emits it within a few reconcile intervals
for _ in $(seq 1 10); do has_event phase_changed to running && break; sleep 1; done
has_event phase_changed to running && ok "'phase_changed → running' emitted on boot" || bad "no boot phase event"

echo "== 04 sleep → asleep event; wake → running event with wake_latency_ms"
cli app sleep web >/dev/null 2>&1
wait_phase web asleep >/dev/null 2>&1 || bad "web did not sleep"
has_event phase_changed to asleep && ok "'phase_changed → asleep' emitted on sleep" || bad "no asleep event"
cli app wake web >/dev/null 2>&1
wait_phase web running >/dev/null 2>&1 || bad "web did not wake"
for _ in $(seq 1 5); do has_wake && break; sleep 1; done
has_wake && ok "wake emitted 'phase_changed → running' with wake_latency_ms" || bad "no wake event with latency"

echo "== 05 update → an 'updated' event"
cli app update web --image "$IMAGE" --pull missing --port 80 --restart always --health "http:80:/" --vcpus 1 --memory 384 >/dev/null 2>&1
has_event updated && ok "'updated' event emitted" || bad "no 'updated' event"

echo "== 06 cursors are strictly monotonic"
events_json | python3 -c '
import json,sys
seqs=[e["seq"] for e in json.load(sys.stdin).get("events",[])]
sys.exit(0 if seqs==sorted(seqs) and len(seqs)==len(set(seqs)) else 1)' \
  && ok "event seqs strictly increasing ($(events_json | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("events",[])))') events)" \
  || bad "event seqs not strictly monotonic"

echo "== 07 ?since=<cursor> replays only newer events"
C="$(cursor)"                       # cursor now, before the delete
cli app rm web >/dev/null 2>&1
sleep 1
# The resumed batch must contain the delete and NOTHING at or before the cursor.
events_json "$C" | python3 -c '
import json,sys
c=int(sys.argv[1]); evs=json.load(sys.stdin).get("events",[])
if any(e["seq"]<=c for e in evs): sys.exit(1)          # nothing old replayed
if not any(e.get("type")=="deleted" for e in evs): sys.exit(2)  # the new delete is there
sys.exit(0)' "$C" \
  && ok "resume from cursor $C returned only newer events (incl. 'deleted')" \
  || bad "?since resume replayed old events or missed the delete"

echo "==============================================================="
echo " events smoke: $pass passed, $fail failed"
echo " transcripts: $MOUNT (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$fail" -eq 0 ]]
