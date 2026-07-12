#!/usr/bin/env bash
#
# Resource- and data-leak acceptance for crucible.
#
# Runs a private daemon (proxy + app→app networking enabled) and drives every
# lifecycle path that could leak a Firecracker VM or bleed data across an
# isolation boundary, asserting after each that nothing was orphaned or exposed.
# The truest "is a VM leaking?" signal is the live firecracker PROCESS count, so
# most checks assert on that (plus the daemon's /sandboxes record count).
#
# RESOURCE LEAKS (no orphaned instances):
#   01 baseline: a fresh daemon owns zero VMs
#   02 rolling `app update`: the OLD instance is reaped after the drain window —
#      exactly one VM remains, not two
#   03 `app rm` mid-drain: deleting an app while its old instance is still
#      draining tears BOTH down (the reconcile teardown must not forget the
#      draining/incoming slots) — zero VMs, no orphan
#   04 superseding `app update`: a second update that flips inside the first's
#      drain window destroys the first's draining instance (single drain slot)
#   05 horizontal scale-out: min-scale N → N VMs; lowering it destroys the extras
#   06 sleep: sleeping mid-drain frees BOTH the current (snapshot) and the
#      draining VM — an asleep app runs zero VMs; wake brings exactly one back
#
# DATA LEAKS (no cross-boundary exposure):
#   07 cross-app filesystem: a secret written in app A's rootfs is absent from a
#      second app B's rootfs (separate per-VM disks, no shared writable layer)
#   08 cross-app network: B cannot reach A's guest IP directly (per-sandbox netns
#      isolation holds even with app→app networking enabled)
#   09 fork clone-safety: two forks of one snapshot get distinct machine-id and
#      distinct kernel-RNG UUIDs (no shared secrets/entropy)
#   10 fork COW isolation: a write in one fork is invisible to its sibling
#      (copy-on-write, no write bleed between clones)
#   11 fresh-rootfs hygiene: a new sandbox does NOT inherit a prior (deleted)
#      sandbox's writes — the rootfs template is clean, not reused dirty
#   12 deleted app: a removed app serves nothing residual (published port dead,
#      proxy → 404)
#
# FINAL: after all cleanup, zero VMs and zero sandbox records remain.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl,
# python3, and internet (pulls nginx:alpine + alpine) or cached images.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_leaks.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7899}"
PROXY_PORT="${PROXY_PORT:-7900}"
DOMAIN="${DOMAIN:-apps.local}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
ALPINE="${ALPINE:-alpine:latest}"
PUB_PORT="${PUB_PORT:-8095}"
# drainWindow in the daemon is 10s; wait comfortably past it when asserting a
# drained instance is gone.
DRAIN_WAIT="${DRAIN_WAIT:-14}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-leaks-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible resource + data leak smoke"
echo " output: $SMOKE_ROOT   proxy: 127.0.0.1:$PROXY_PORT ($DOMAIN)"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null    || { echo "error: curl needed" >&2; exit 2; }
command -v python3 >/dev/null || { echo "error: python3 needed" >&2; exit 2; }
command -v pgrep >/dev/null   || { echo "error: pgrep needed" >&2; exit 2; }
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }
api()  { curl -s --max-time 5 "$@"; }
pyget() { python3 -c "import json,sys; d=json.load(sys.stdin); print(d$1)" 2>/dev/null; }

# fc_count — live Firecracker VMs owned by this host (the daemon is private, so
# these are exactly the ones this smoke created). The core leak metric.
fc_count() { local n; n="$(pgrep -c firecracker 2>/dev/null)"; echo "${n:-0}"; }
# sbx_total — sandbox RECORDS the daemon tracks (a draining orphan shows here too).
sbx_total() { api "$BASE_URL/sandboxes" 2>/dev/null | grep -o '"id":"sbx_[a-z0-9]*"' | wc -l | tr -d ' '; }

phase()    { cli app get "$1" 2>/dev/null | pyget '.get("status",{}).get("phase","")'; }
app_inst() { cli app get "$1" 2>/dev/null | pyget '.get("status",{}).get("instance_id","")'; }
guest_ip() { api "$BASE_URL/sandboxes/$1" 2>/dev/null | grep -o '"guest_ip":"[0-9.]*"' | grep -o '[0-9.]*' | head -1; }
exec_app() { local app="$1"; shift; cli app exec "$app" -- "$@" 2>/dev/null; }
exec_sbx() { local id="$1"; shift; cli sandbox exec "$id" -- "$@" 2>/dev/null; }

wait_phase() { for _ in {1..80}; do [[ "$(phase "$1")" == "$2" ]] && return 0; sleep 0.5; done; return 1; }
# wait_fc <n> [<tries>] — poll until fc_count == n.
wait_fc() { local want="$1" tries="${2:-40}"; for _ in $(seq 1 "$tries"); do [[ "$(fc_count)" -eq "$want" ]] && return 0; sleep 0.5; done; return 1; }
# wait_flip <app> <old-inst> — poll until the app's current instance != old.
wait_flip() { local app="$1" old="$2" cur; for _ in {1..80}; do cur="$(app_inst "$app")"; [[ -n "$cur" && "$cur" != "$old" ]] && return 0; sleep 0.5; done; return 1; }
hit() { local url="$1" needle="$2" body; for _ in {1..40}; do body="$(curl -s --max-time 3 "$url" 2>/dev/null || true)"; [[ "$body" == *"$needle"* ]] && return 0; sleep 0.5; done; return 1; }

# ---- daemon (proxy + app→app networking) ------------------------------------
DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --network-egress-iface "$EGRESS_IFACE" \
    --proxy-listen "127.0.0.1:$PROXY_PORT" --proxy-domain "$DOMAIN" \
    --internal-networking \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}
cleanup() {
  for a in web webb up sleeper resid; do cli app rm "$a" >/dev/null 2>&1 || true; done
  sleep 1
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

# Pre-pull the images once so per-check timing isn't dominated by conversion.
echo "== 00 start daemon + warm the image cache"
start_daemon
cli image pull "$IMAGE"  >/dev/null 2>&1 || true
cli image pull "$ALPINE" >/dev/null 2>&1 || true

# =============================================================================
# RESOURCE LEAKS
# =============================================================================
echo "== 01 baseline: a fresh daemon owns zero VMs"
if [[ "$(fc_count)" -eq 0 && "$(sbx_total)" -eq 0 ]]; then
  pass "baseline clean (0 firecracker, 0 sandbox records)"
else
  fail "not clean at start: fc=$(fc_count) sbx=$(sbx_total)"
fi

echo "== 02 rolling update reaps the old instance (no 2nd VM lingers)"
cli app create web --image "$IMAGE" --pull missing --port 80 --restart always \
  --health "http:80:/" --memory 256 >/dev/null 2>&1
if ! wait_phase web running || ! wait_fc 1; then
  fail "web never came up (fc=$(fc_count))"; tail -30 "$DAEMON_LOG"
else
  OLD="$(app_inst web)"
  upd_memory=320
  cli app update web --image "$IMAGE" --port 80 --restart always --health "http:80:/" --memory "$upd_memory" >/dev/null 2>&1
  if wait_flip web "$OLD"; then
    # right after the flip both the new and the draining old VM are live
    if [[ "$(fc_count)" -ge 2 ]]; then
      echo "   (flip observed: fc=$(fc_count), old $OLD draining)"
    fi
    if wait_fc 1 $((DRAIN_WAIT*2)); then
      pass "old instance reaped after drain window — exactly 1 VM remains"
    else
      fail "rolling update leaked a VM: fc=$(fc_count) after drain (old $OLD orphaned?)"
    fi
  else
    fail "update never flipped to a new instance"
  fi
fi

echo "== 03 app rm mid-drain tears down BOTH current and draining instance"
# Trigger another roll, then delete the app WHILE the old instance is draining.
OLD2="$(app_inst web)"
cli app update web --image "$IMAGE" --port 80 --restart always --health "http:80:/" --memory 256 >/dev/null 2>&1
if wait_flip web "$OLD2"; then
  # within the 10s drain window now — delete and assert everything is reaped fast
  cli app rm web >/dev/null 2>&1
  if wait_fc 0 20; then
    pass "app rm mid-drain reaped every instance (0 VMs, no drain orphan)"
  else
    fail "app rm mid-drain leaked: fc=$(fc_count) (draining $OLD2 orphaned)"
  fi
else
  fail "second update never flipped (mid-drain rm not exercised)"
  cli app rm web >/dev/null 2>&1
fi
wait_fc 0 10 >/dev/null || true

echo "== 04 superseding update reaps the prior draining instance"
cli app create web --image "$IMAGE" --pull missing --port 80 --restart always --health "http:80:/" --memory 256 >/dev/null 2>&1
wait_phase web running >/dev/null && wait_fc 1 >/dev/null
A0="$(app_inst web)"
cli app update web --image "$IMAGE" --port 80 --restart always --health "http:80:/" --memory 300 >/dev/null 2>&1
if wait_flip web "$A0"; then
  A1="$(app_inst web)"   # A0 now draining
  # Second update immediately (inside the 10s window) — must reap A0, not orphan it.
  cli app update web --image "$IMAGE" --port 80 --restart always --health "http:80:/" --memory 260 >/dev/null 2>&1
  if wait_flip web "$A1"; then
    # after two flips inside the window: 1 current + 1 draining = 2 VMs, NOT 3.
    sleep 2
    if [[ "$(fc_count)" -le 2 ]]; then
      pass "superseding update kept ≤2 VMs (prior draining instance reaped, not orphaned)"
    else
      fail "superseding update orphaned an instance: fc=$(fc_count), want ≤2"
    fi
    wait_fc 1 $((DRAIN_WAIT*2)) >/dev/null && echo "   (settled back to 1 VM after drain)"
  else
    fail "second (superseding) update never flipped"
  fi
else
  fail "first update never flipped"
fi
cli app rm web >/dev/null 2>&1; wait_fc 0 20 >/dev/null || true

echo "== 05 horizontal scale-out: extras are destroyed when the floor drops"
cli app create up --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 \
  --min-scale 3 --max-scale 4 >/dev/null 2>&1
if wait_fc 3 60; then
  pass "min-scale 3 converged to 3 VMs"
  cli app update up --image "$IMAGE" --port 80 --restart always --memory 256 --min-scale 1 --max-scale 4 >/dev/null 2>&1
  if wait_fc 1 60; then
    pass "lowering the floor to 1 destroyed the 2 extra VMs (no leak)"
  else
    fail "scale-down leaked: fc=$(fc_count), want 1"
  fi
else
  fail "min-scale 3 did not converge: fc=$(fc_count)"
fi
cli app rm up >/dev/null 2>&1; wait_fc 0 30 >/dev/null || true

echo "== 06 sleep frees the current AND any draining VM; wake brings back exactly one"
cli app create sleeper --image "$IMAGE" --pull missing -p "${PUB_PORT}:80" --restart always \
  --health "http:80:/" --memory 256 >/dev/null 2>&1
wait_phase sleeper running >/dev/null && wait_fc 1 >/dev/null
SOLD="$(app_inst sleeper)"
cli app update sleeper --image "$IMAGE" -p "${PUB_PORT}:80" --restart always --health "http:80:/" --memory 300 >/dev/null 2>&1
if wait_flip sleeper "$SOLD"; then
  # sleep while SOLD is still draining
  cli app sleep sleeper >/dev/null 2>&1
  if wait_phase sleeper asleep >/dev/null && wait_fc 0 20; then
    pass "asleep app runs 0 VMs (current snapshotted, draining instance reaped)"
  else
    fail "sleep left VM(s) running: fc=$(fc_count) phase=$(phase sleeper)"
  fi
  cli app wake sleeper >/dev/null 2>&1
  if wait_phase sleeper running >/dev/null && wait_fc 1 30; then
    pass "wake restored exactly one VM"
  else
    fail "wake did not restore exactly one VM: fc=$(fc_count)"
  fi
else
  fail "sleeper update never flipped (sleep-mid-drain not exercised)"
fi
cli app rm sleeper >/dev/null 2>&1; wait_fc 0 20 >/dev/null || true

# =============================================================================
# DATA LEAKS / ISOLATION
# =============================================================================
echo "== 07 cross-app filesystem isolation (separate per-VM rootfs)"
SECRET="leak-canary-$$-$RANDOM"
cli app create web  --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 >/dev/null 2>&1
cli app create webb --image "$IMAGE" --pull missing --port 80 --restart always --memory 256 >/dev/null 2>&1
if wait_phase web running >/dev/null && wait_phase webb running >/dev/null; then
  exec_app web sh -c "echo $SECRET > /root/canary" >/dev/null 2>&1
  GOT="$(exec_app web  cat /root/canary 2>/dev/null)"
  BLEED="$(exec_app webb cat /root/canary 2>/dev/null)"
  if [[ "$GOT" == *"$SECRET"* && "$BLEED" != *"$SECRET"* ]]; then
    pass "app A's secret is present in A, ABSENT from app B (no rootfs bleed)"
  else
    fail "cross-app fs leak: A='${GOT:0:20}' B='${BLEED:0:20}'"
  fi
else
  fail "web/webb did not both come up for the fs-isolation check"
fi

echo "== 08 cross-app network isolation (B cannot reach A's guest IP directly)"
AIP="$(guest_ip "$(app_inst web)")"
if [[ -n "$AIP" ]]; then
  if exec_app webb sh -c "nc -w 3 $AIP 80 </dev/null" >/dev/null 2>&1; then
    fail "SECURITY: app B reached app A's guest IP $AIP directly — lateral isolation broken"
  else
    pass "app B cannot reach app A's guest IP $AIP directly (netns isolation holds)"
  fi
else
  fail "could not read app A's guest IP for the isolation check"
fi
cli app rm web  >/dev/null 2>&1
cli app rm webb >/dev/null 2>&1
wait_fc 0 20 >/dev/null || true

echo "== 09 fork clone-safety: forks get distinct machine-id + kernel-RNG uuid"
SBX="$(cli sandbox create --image "$ALPINE" --memory 256 2>/dev/null)"
if [[ "$SBX" == sbx_* ]]; then
  SNAP="$(cli snapshot create "$SBX" 2>/dev/null)"
  FORKS="$(cli fork "$SNAP" --count 2 2>/dev/null)"
  F1="$(echo "$FORKS" | grep -o 'sbx_[a-z0-9]*' | sed -n 1p)"
  F2="$(echo "$FORKS" | grep -o 'sbx_[a-z0-9]*' | sed -n 2p)"
  if [[ "$F1" == sbx_* && "$F2" == sbx_* ]]; then
    MID1="$(exec_sbx "$F1" cat /etc/machine-id 2>/dev/null | tr -d '[:space:]')"
    MID2="$(exec_sbx "$F2" cat /etc/machine-id 2>/dev/null | tr -d '[:space:]')"
    U1="$(exec_sbx "$F1" cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d '[:space:]')"
    U2="$(exec_sbx "$F2" cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d '[:space:]')"
    if [[ -n "$MID1" && "$MID1" != "$MID2" ]]; then
      pass "forks have distinct machine-id ($MID1 != $MID2)"
    else
      fail "forks SHARE machine-id ('$MID1' == '$MID2') — identity leak"
    fi
    if [[ -n "$U1" && "$U1" != "$U2" ]]; then
      pass "forks draw distinct kernel-RNG uuids (entropy not shared)"
    else
      fail "forks drew the SAME uuid ('$U1') — CRNG not reseeded per fork"
    fi

    echo "== 10 fork COW isolation: a write in one fork is invisible to its sibling"
    exec_sbx "$F1" sh -c 'echo fork1-only > /tmp/fork-write' >/dev/null 2>&1
    SIB="$(exec_sbx "$F2" sh -c 'cat /tmp/fork-write 2>/dev/null || echo ABSENT' 2>/dev/null)"
    if [[ "$SIB" == *ABSENT* ]]; then
      pass "a write in fork 1 is not visible in fork 2 (copy-on-write isolation)"
    else
      fail "fork write bled to sibling: fork2 saw '${SIB:0:30}'"
    fi
  else
    fail "fork did not produce two sandboxes: $FORKS"
  fi
  cli snapshot rm "$SNAP" >/dev/null 2>&1 || true
  [[ "$F1" == sbx_* ]] && cli sandbox rm "$F1" >/dev/null 2>&1
  [[ "$F2" == sbx_* ]] && cli sandbox rm "$F2" >/dev/null 2>&1
  cli sandbox rm "$SBX" >/dev/null 2>&1
else
  fail "could not create a sandbox for the fork checks"
fi
wait_fc 0 20 >/dev/null || true

echo "== 11 fresh-rootfs hygiene: a new sandbox does not inherit a deleted one's writes"
S1="$(cli sandbox create --image "$ALPINE" --memory 256 2>/dev/null)"
if [[ "$S1" == sbx_* ]]; then
  MARK="dirty-$$-$RANDOM"
  exec_sbx "$S1" sh -c "echo $MARK > /root/leftover" >/dev/null 2>&1
  cli sandbox rm "$S1" >/dev/null 2>&1
  sleep 1
  S2="$(cli sandbox create --image "$ALPINE" --memory 256 2>/dev/null)"
  if [[ "$S2" == sbx_* ]]; then
    LEFT="$(exec_sbx "$S2" sh -c 'cat /root/leftover 2>/dev/null || echo CLEAN' 2>/dev/null)"
    if [[ "$LEFT" == *CLEAN* ]]; then
      pass "new sandbox booted a clean rootfs (no data from the deleted sandbox)"
    else
      fail "rootfs data bled across sandboxes: new sandbox saw '${LEFT:0:30}'"
    fi
    cli sandbox rm "$S2" >/dev/null 2>&1
  else
    fail "could not create the second sandbox for the hygiene check"
  fi
else
  fail "could not create the first sandbox for the hygiene check"
fi
wait_fc 0 20 >/dev/null || true

echo "== 12 a deleted app serves nothing residual"
cli app create resid --image "$IMAGE" --pull missing -p "${PUB_PORT}:80" --port 80 --restart always --memory 256 >/dev/null 2>&1
if wait_phase resid running >/dev/null && hit "http://localhost:${PUB_PORT}/" "nginx"; then
  cli app rm resid >/dev/null 2>&1
  sleep 3
  DEADP=1; DEADX=1
  curl -sf "http://localhost:${PUB_PORT}/" >/dev/null 2>&1 && DEADP=0
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 4 -H "Host: resid.${DOMAIN}" "http://127.0.0.1:${PROXY_PORT}/" 2>/dev/null)"
  [[ "$code" == "404" ]] || DEADX=0
  if [[ "$DEADP" -eq 1 && "$DEADX" -eq 1 ]]; then
    pass "deleted app: published port dead + proxy → 404 (nothing residual)"
  else
    fail "deleted app still reachable (port_dead=$DEADP proxy_404=$DEADX code=$code)"
  fi
else
  fail "resid app never served for the deletion check"
fi
wait_fc 0 20 >/dev/null || true

# =============================================================================
echo "== 13 FINAL: no orphaned VMs or sandbox records after everything"
FC="$(fc_count)"; SBX_N="$(sbx_total)"
if [[ "$FC" -eq 0 && "$SBX_N" -eq 0 ]]; then
  pass "clean exit: 0 firecracker VMs, 0 sandbox records"
else
  fail "leaked after full run: fc=$FC sandbox-records=$SBX_N"
  echo "   surviving sandboxes:"; cli sandbox ls 2>/dev/null | sed 's/^/     /'
fi

echo "==============================================================="
echo " leak smoke: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
