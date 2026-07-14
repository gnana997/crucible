#!/usr/bin/env bash
#
# Upgrade-without-drop rehearsal (docs/upgrades.md), run by robots every
# release so cross-version compatibility is measured, not assumed.
#
# Builds the PREVIOUS release from its git tag, then walks the runbook:
#
#   01  the previous release's binary is built from its tag (OLD_REF; override
#       with OLD_BIN=/path to skip the build)
#   02  the OLD daemon runs two apps over shared state dirs: a stateless nginx
#       (published port) and a volume-backed redis with a marker file written
#       into its volume
#   03  drain: `app sleep --all` (the NEW CLI, against the old daemon) puts
#       every app to sleep — zero VMs, durable snapshots on disk
#   04  the upgrade: old daemon stops, the NEW (HEAD) daemon starts over the
#       same state dirs and RE-ADOPTS both apps as asleep, still zero VMs
#   05  both apps wake under the new binary and serve; the volume marker
#       survived. The wake path is REPORTED (warm restore from the old-agent
#       snapshot vs the automatic cold fallback): warm is expected; a fallback
#       is a compat regression to investigate before release, and a wake that
#       fails BOTH ways fails the smoke.
#
# The subtle thing under test: a durable sleep snapshot contains the PREVIOUS
# release's guest agent frozen in memory; waking it under the new daemon
# exercises cross-version agent-protocol compatibility.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built (HEAD),
# git + go (to build the old release), curl, python3, and internet (pulls
# nginx:alpine + redis:alpine) or cached images.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_upgrade.sh

set -u
set -o pipefail

NEW_BIN="${CRUCIBLE_BIN:-./crucible}"
OLD_BIN="${OLD_BIN:-}"
OLD_REF="${OLD_REF:-}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
LISTEN="${LISTEN:-127.0.0.1:7898}"
BASE_URL="http://${LISTEN}"
NGINX_IMAGE="${NGINX_IMAGE:-nginx:alpine}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:alpine}"
HP_WEB="${HP_WEB:-7931}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-upgrade-$(date +%Y%m%d-%H%M%S)}"
# The volume dir must share a filesystem with the jailer chroot base (a
# persistent volume HARDLINKS into the jail; cross-fs is rejected by design),
# and /tmp is often a different fs (or tmpfs) — so both live under a dedicated
# data root on /var/lib, wiped per run. Transcripts stay in SMOKE_ROOT.
DATA_ROOT="${DATA_ROOT:-/var/lib/crucible-smoke-upgrade}"
CHROOT_BASE="${CHROOT_BASE:-$DATA_ROOT/jailer}"
VOL_DIR="$DATA_ROOT/volumes"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"
APP_DB="$SMOKE_ROOT/apps.db"
OLD_LOG="$SMOKE_ROOT/daemon-old.log"; NEW_LOG="$SMOKE_ROOT/daemon-new.log"
rm -rf "$DATA_ROOT"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR" "$VOL_DIR" "$CHROOT_BASE"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible upgrade-without-drop rehearsal"
echo " output dir : $SMOKE_ROOT"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$NEW_BIN" ]] || { echo "error: $NEW_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null    || { echo "error: curl needed" >&2; exit 2; }
command -v python3 >/dev/null || { echo "error: python3 needed" >&2; exit 2; }
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }
if systemctl is-active --quiet crucible 2>/dev/null; then
  echo "error: systemd crucible is active — stop it first (this starts its own daemon)" >&2; exit 2
fi

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
# Building as the repo owner (not root) keeps .git free of root-owned files.
BUILD_AS=()
if [[ -n "${SUDO_USER:-}" && "$SUDO_USER" != "root" ]]; then BUILD_AS=(sudo -u "$SUDO_USER"); fi
cli() { "$NEW_BIN" --addr "$LISTEN" "$@"; }   # the NEW CLI talks to both daemons
phase_of() { cli app get "$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
wait_phase_of() { local app="$1" want="$2" tries="${3:-240}"; for _ in $(seq 1 "$tries"); do [[ "$(phase_of "$app")" == "$want" ]] && return 0; sleep 0.5; done; return 1; }
fc_count() { pgrep -f 'firecracker --id' 2>/dev/null | wc -l | tr -d ' '; }
wait_fc() { local want="$1" tries="${2:-60}"; for _ in $(seq 1 "$tries"); do [[ "$(fc_count)" -eq "$want" ]] && return 0; sleep 0.5; done; return 1; }

DAEMON_PID=""
start_daemon() { # $1 = binary, $2 = log file
  "$1" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --volume-dir "$VOL_DIR" \
    --network-egress-iface "$EGRESS_IFACE" \
    --log-format json --log-level info >>"$2" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$2"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$2"; exit 3
}
stop_daemon() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  DAEMON_PID=""
}
WORKTREE=""
cleanup() {
  if [[ -n "$DAEMON_PID" ]]; then
    cli app rm web >/dev/null 2>&1 || true
    cli app rm data >/dev/null 2>&1 || true
    sleep 1
    cli volume rm data1 >/dev/null 2>&1 || true
    stop_daemon
  fi
  if [[ -n "$WORKTREE" ]]; then
    "${BUILD_AS[@]}" git worktree remove --force "$WORKTREE" >/dev/null 2>&1 || true
    "${BUILD_AS[@]}" git worktree prune >/dev/null 2>&1 || true
  fi
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$DATA_ROOT"
}
trap cleanup EXIT

echo "== 01 build the previous release"
if [[ -z "$OLD_BIN" ]]; then
  # --match 'v*' pins this to daemon release tags (vX.Y.Z) — the repo also
  # carries nested-module tags like sdk/v0.6.0 that describe would prefer
  # when newer.
  OLD_REF="${OLD_REF:-$("${BUILD_AS[@]}" git describe --tags --match 'v*' --abbrev=0 2>/dev/null)}"
  [[ -n "$OLD_REF" ]] || { echo "error: no previous tag found; set OLD_REF or OLD_BIN" >&2; exit 2; }
  WORKTREE="$SMOKE_ROOT/old-src"
  # SMOKE_ROOT is root-owned; hand the (empty, allowed) worktree dir to the
  # build user so git can populate it.
  if [[ ${#BUILD_AS[@]} -gt 0 ]]; then
    install -d -o "$SUDO_USER" "$WORKTREE"
  else
    mkdir -p "$WORKTREE"
  fi
  if ! "${BUILD_AS[@]}" git worktree add --detach "$WORKTREE" "$OLD_REF" >"$SMOKE_ROOT/old-build.log" 2>&1; then
    echo "error: git worktree add $OLD_REF failed:" >&2
    cat "$SMOKE_ROOT/old-build.log" >&2; exit 2
  fi
  echo "   building $OLD_REF (previous release) ..."
  if ! (cd "$WORKTREE" && "${BUILD_AS[@]}" make build >>"$SMOKE_ROOT/old-build.log" 2>&1); then
    echo "error: building $OLD_REF failed:" >&2
    tail -20 "$SMOKE_ROOT/old-build.log" >&2; exit 2
  fi
  OLD_BIN="$WORKTREE/crucible"
fi
[[ -x "$OLD_BIN" ]] || { echo "error: old binary $OLD_BIN not executable" >&2; exit 2; }
echo "   old: $("$OLD_BIN" version 2>/dev/null | head -1)"
echo "   new: $("$NEW_BIN" version 2>/dev/null | head -1)"
pass "previous release built (${OLD_REF:-$OLD_BIN})"

FC0="$(fc_count)"

echo "== 02 OLD daemon: stateless app + volume app with a marker"
start_daemon "$OLD_BIN" "$OLD_LOG"
cli app create web --image "$NGINX_IMAGE" --pull missing -p "$HP_WEB:80" \
  --restart always --health "http:80:/" --memory 256 >/dev/null 2>&1
cli app create data --image "$REDIS_IMAGE" --pull missing \
  --restart always --health "tcp:6379" --memory 256 \
  --volume data1:/data >/dev/null 2>&1
if wait_phase_of web running && wait_phase_of data running && wait_fc $((FC0+2)); then
  pass "both apps running under $("$OLD_BIN" version 2>/dev/null | head -1)"
else
  fail "apps never ran under the old daemon: web=$(phase_of web) data=$(phase_of data)"; tail -20 "$OLD_LOG"; exit 1
fi
cli app exec data -- sh -c 'echo upgraded-marker > /data/marker && sync' >/dev/null 2>&1
MARK="$(cli app exec data -- cat /data/marker 2>/dev/null | tr -d '[:space:]')"
[[ "$MARK" == "upgraded-marker" ]] && pass "marker written to the volume" \
  || fail "could not write/read the volume marker: '$MARK'"

echo "== 03 drain: app sleep --all puts the fleet to sleep"
if cli app sleep --all >/dev/null 2>&1 \
  && [[ "$(phase_of web)" == "asleep" && "$(phase_of data)" == "asleep" ]] \
  && wait_fc "$FC0" 40; then
  pass "drained: both apps asleep, zero VMs"
else
  fail "drain failed: web=$(phase_of web) data=$(phase_of data) fc=$(fc_count)"
fi

echo "== 04 the upgrade: swap daemons over the same state dirs"
stop_daemon
start_daemon "$NEW_BIN" "$NEW_LOG"
if [[ "$(phase_of web)" == "asleep" && "$(phase_of data)" == "asleep" ]] && [[ "$(fc_count)" -eq "$FC0" ]]; then
  pass "new daemon re-adopted both apps asleep (no VMs, no cold boots)"
else
  fail "re-adoption wrong: web=$(phase_of web) data=$(phase_of data) fc=$(fc_count)"
fi

echo "== 05 wake under the new binary: apps serve, data intact"
cli app wake web >/dev/null 2>&1
cli app wake data >/dev/null 2>&1
WOKE=1
wait_phase_of web running 120 || { WOKE=0; fail "web did not wake under the new daemon"; }
wait_phase_of data running 120 || { WOKE=0; fail "data did not wake under the new daemon"; }
if [[ "$WOKE" -eq 1 ]]; then
  pass "both apps woke under the new daemon"
  SERVED=0
  for _ in $(seq 1 30); do
    B="$(curl -s --max-time 3 "http://127.0.0.1:$HP_WEB/" 2>/dev/null || true)"
    [[ "$B" == *nginx* || "$B" == *html* ]] && { SERVED=1; break; }
    sleep 0.5
  done
  [[ "$SERVED" -eq 1 ]] && pass "stateless app serves on its published port" \
    || fail "web woke but never served on :$HP_WEB"
  MARK2="$(cli app exec data -- cat /data/marker 2>/dev/null | tr -d '[:space:]')"
  [[ "$MARK2" == "upgraded-marker" ]] && pass "volume marker survived the upgrade" \
    || fail "volume marker lost across the upgrade: '$MARK2'"
  # Which wake path ran? Warm restore is the goal; the automatic fallback is
  # a cross-version compat regression to investigate (not a smoke failure —
  # the apps ARE up — but it must be a conscious release decision).
  if grep -q "falling back to stop/start cold-create\|asleep app snapshot missing" "$NEW_LOG"; then
    echo "   NOTE: wake used the COLD fallback path — old-agent warm snapshots did not"
    echo "         restore under this binary. Investigate before release (see docs/upgrades.md)."
  else
    pass "wakes were WARM restores of the old release's snapshots (full compat)"
  fi
fi

cli app rm web >/dev/null 2>&1; cli app rm data >/dev/null 2>&1; sleep 1
cli volume rm data1 >/dev/null 2>&1

echo "==============================================================="
echo " upgrade rehearsal: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (old: $OLD_LOG, new: $NEW_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
