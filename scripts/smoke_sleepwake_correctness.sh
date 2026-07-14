#!/usr/bin/env bash
#
# Correctness matrix for app sleep/wake (v0.5.0). Where
# smoke_app_sleepwake.sh proves scale-to-zero *works*, this proves it is
# *correct* — the two silent-corruption gates the functional smoke can't see:
#
#   * guest clock is STEPPED on wake. After a real sleep gap, an asleep
#            guest's clock is frozen at snapshot time; wake steps it to host
#            wall time. We sleep the app for GAP seconds, wake it, and assert the
#            guest's clock is within a few seconds of the host's — NOT ~GAP
#            behind (which is what a frozen, un-stepped clock would show).
#
#   * identity is PRESERVED across wake (wake != fork). Wake reseeds the
#            CRNG but must NOT rotate hostname/machine-id. We capture the guest
#            hostname before sleep and assert it is unchanged after wake.
#
# A guest TCP listener surviving restore, and the slept instance's
# netns not being reaped, are already demonstrated by smoke_app_sleepwake.sh: the
# app serves again on wake (listener survived) at the same guest IP (netns kept).
#
# v0.6.4 extends the matrix with the connection-semantics rows (a raw-TCP redis
# behind the L4 waking forwarder):
#
#   * a connection ESTABLISHED at snapshot time is a dead peer, bounded.
#            Forced sleep must complete with a client connected, and that client's
#            socket must be CLOSED within the connection reap window — never
#            preserved, never left to hang (a snapshot-stopped guest black-holes
#            the pipe; the reaper is what bounds it).
#
#   * the WAKING connection itself carries bytes. A client that connects
#            to a slept app and immediately sends a command must get the right
#            reply: the forwarder reads no client bytes before the backend dial,
#            so the command waits in the socket across the wake. Also proves the
#            value written before sleep survived (in-place snapshot wake).
#
#   * a request racing `app sleep` converges. Sleep and wake serialize on
#            the app's transition lock, so a connection landing mid-snapshot must
#            be served (sleep completes, then wake restores) — and never leaves
#            zero or two VMs behind.
#
#   * a wake herd is one wake. N concurrent connections to a slept app
#            are all served by exactly one restored VM (end-to-end check of the
#            coalescing the unit test proves in-process).
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built, curl,
# python3, and internet (pulls nginx:alpine + redis:alpine) or cached images.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_sleepwake_correctness.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7893}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:alpine}"
HP_R="${HP_R:-7897}"      # published host port for the connection-semantics redis
GAP="${GAP:-15}"          # seconds asleep — must exceed CLOCK_TOL to be meaningful
CLOCK_TOL="${CLOCK_TOL:-5}"

SMOKE_ROOT="${SMOKE_ROOT:-/tmp/crucible-smoke-sw-correctness-$(date +%Y%m%d-%H%M%S)}"
IMAGE_DIR="$SMOKE_ROOT/images"; WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"; APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"
exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible sleep/wake correctness (v0.5.0)"
echo " output dir : $SMOKE_ROOT   gap: ${GAP}s   clock tol: ${CLOCK_TOL}s"
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
EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }
if systemctl is-active --quiet crucible 2>/dev/null; then
  echo "error: systemd crucible is active — stop it first (this starts its own daemon)" >&2; exit 2
fi

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }
phase() { cli app get web 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
# gexec <cmd...> — run a command in the app's current instance, return trimmed stdout.
gexec() { cli app exec web -- "$@" 2>/dev/null | tr -d '\r' | tail -1 | tr -d '[:space:]'; }
wait_phase() { for _ in {1..60}; do [[ "$(phase)" == "$1" ]] && return 0; sleep 0.5; done; return 1; }

# ---- connection-semantics helpers (the redis rows) --------------------------
phase_of() { cli app get "$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("phase",""))' 2>/dev/null; }
inst_of() { cli app get "$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",{}).get("instance_id",""))' 2>/dev/null; }
wait_phase_of() { local app="$1" want="$2" tries="${3:-120}"; for _ in $(seq 1 "$tries"); do [[ "$(phase_of "$app")" == "$want" ]] && return 0; sleep 0.5; done; return 1; }
fc_count() { pgrep -f 'firecracker --id' 2>/dev/null | wc -l | tr -d ' '; }
wait_fc() { local want="$1" tries="${2:-60}"; for _ in $(seq 1 "$tries"); do [[ "$(fc_count)" -eq "$want" ]] && return 0; sleep 0.5; done; return 1; }

# redis_cmd PORT TIMEOUT CMD... — open a connection to the published port, send
# ONE inline redis command immediately (before any reply can arrive — on a slept
# app these bytes wait in the socket across the wake), print the raw reply.
redis_cmd() {
  local port="$1" tmo="$2"; shift 2
  python3 - "$port" "$tmo" "$@" <<'PY'
import socket, sys
port, tmo = int(sys.argv[1]), float(sys.argv[2])
try:
    s = socket.create_connection(("127.0.0.1", port), timeout=tmo)
    s.settimeout(tmo)
    s.sendall((" ".join(sys.argv[3:]) + "\r\n").encode())
    data = b""
    # inline replies are one CRLF line; bulk replies ($N) are two
    while data.count(b"\r\n") < (2 if data.startswith(b"$") else 1):
        chunk = s.recv(4096)
        if not chunk:
            break
        data += chunk
    s.close()
    sys.stdout.write(data.decode(errors="replace"))
except Exception as e:
    sys.stderr.write(f"{e}\n")
    sys.exit(1)
PY
}

# hold_conn PORT FLAGFILE OUTFILE — establish a connection, prove it live with a
# PING (so it is ESTABLISHED, with recent bytes, when the snapshot fires), touch
# FLAGFILE, then block on recv and record how the socket ENDS in OUTFILE:
# eof/reset = peer closed it (correct: dead peers are bounded) vs timeout =
# black-holed (a wedge — the failure this row exists to catch). Backgrounded.
HOLDER_PID=""
hold_conn() {
  python3 - "$1" "$2" <<'PY' >"$3" 2>/dev/null &
import socket, sys
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=10)
s.sendall(b"PING\r\n")
s.recv(64)                      # reply consumed: live, ESTABLISHED, bytes flowed
open(sys.argv[2], "w").close()  # signal the parent we are holding
s.settimeout(45)
try:
    d = s.recv(1)
    print("eof" if d == b"" else "data")
except socket.timeout:
    print("timeout")
except OSError:
    print("reset")
PY
  HOLDER_PID=$!
}

# ---- daemon + app -----------------------------------------------------------
DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" --network-egress-iface "$EGRESS_IFACE" \
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
  [[ -n "$HOLDER_PID" ]] && kill "$HOLDER_PID" 2>/dev/null
  cli app rm web >/dev/null 2>&1 || true
  cli app rm r1 >/dev/null 2>&1 || true; sleep 1
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== 01 start daemon + create app"
start_daemon
FC0="$(fc_count)"   # firecracker procs before this smoke (usually 0)
if [[ "$(cli app create web --image "$IMAGE" --pull missing --restart always --memory 256 2>/dev/null)" != "web" ]]; then
  fail "app create failed"; tail -30 "$DAEMON_LOG"; exit 1
fi
if ! wait_phase running; then fail "app never became running"; tail -30 "$DAEMON_LOG"; exit 1; fi
# The guest must answer exec (has a shell + coreutils) for the probes below.
HOST_BEFORE="$(gexec hostname)"
if [[ -z "$HOST_BEFORE" ]]; then fail "guest exec returned no hostname"; tail -30 "$DAEMON_LOG"; exit 1; fi
pass "app running; guest hostname=$HOST_BEFORE"

echo "== 02 sleep for ${GAP}s (frozen guest clock accrues drift)"
cli app sleep web >/dev/null 2>&1
if [[ "$(phase)" != "asleep" ]]; then fail "phase=$(phase), want asleep"; exit 1; fi
sleep "$GAP"

echo "== 03 wake"
cli app wake web >/dev/null 2>&1
if ! wait_phase running; then fail "app did not return to running after wake"; tail -40 "$DAEMON_LOG"; exit 1; fi
pass "woke to running"

echo "== 04 identity preserved (hostname unchanged, wake != fork)"
HOST_AFTER="$(gexec hostname)"
if [[ "$HOST_AFTER" == "$HOST_BEFORE" ]]; then
  pass "hostname unchanged across sleep/wake ($HOST_AFTER)"
else
  fail "hostname changed: before=$HOST_BEFORE after=$HOST_AFTER (wake must not rotate identity)"
fi

echo "== 05 guest clock stepped to host time (not ~${GAP}s behind)"
HOST_NOW="$(date +%s)"
GUEST_NOW="$(gexec date +%s)"
if [[ "$GUEST_NOW" =~ ^[0-9]+$ ]]; then
  DIFF=$(( HOST_NOW > GUEST_NOW ? HOST_NOW - GUEST_NOW : GUEST_NOW - HOST_NOW ))
  echo "   host=$HOST_NOW guest=$GUEST_NOW diff=${DIFF}s (tol ${CLOCK_TOL}s; frozen clock would be ~${GAP}s)"
  if [[ "$DIFF" -le "$CLOCK_TOL" ]]; then
    pass "guest clock stepped on wake (drift ${DIFF}s <= ${CLOCK_TOL}s)"
  else
    fail "guest clock NOT stepped: ${DIFF}s drift (stale clock breaks TLS/JWT/TTLs)"
  fi
else
  fail "could not read guest clock: '$GUEST_NOW'"
fi

echo "== 06 raw-TCP app for the connection-semantics rows (redis behind the L4 forwarder)"
# The clock/identity app is done — retire it so VM counting below is exact.
cli app rm web >/dev/null 2>&1 || true
wait_fc "$FC0" 40 || echo "   (warn: VM count did not settle after app rm web)"
# idle-timeout is long (manual sleep drives every transition below) but the
# CONNECTION reap stays short: it is what bounds a dead peer's socket in 07.
if [[ "$(cli app create r1 --image "$REDIS_IMAGE" -p "$HP_R:6379" --restart always \
    --health "tcp:6379" --memory 256 --min-scale 0 --idle-timeout 3600s \
    --connection-idle-timeout 5s 2>/dev/null)" != "r1" ]]; then
  fail "redis app create failed"; tail -20 "$DAEMON_LOG"
else
  if wait_phase_of r1 running 240 && [[ "$(redis_cmd "$HP_R" 20 PING)" == *PONG* ]]; then
    pass "redis serves over the published port (L4 forwarder)"
  else
    fail "redis never served on :$HP_R (phase=$(phase_of r1) fc=$(fc_count))"
  fi
fi

echo "== 07 forced sleep with an ESTABLISHED connection; the dead peer is bounded"
if [[ "$(redis_cmd "$HP_R" 20 SET smoke_k crucible-e2)" == *OK* ]]; then
  pass "wrote smoke_k=crucible-e2 before sleep"
else
  fail "SET before sleep failed"
fi
INST_BEFORE="$(inst_of r1)"
HOLD_FLAG="$SMOKE_ROOT/holder.flag"; HOLD_OUT="$SMOKE_ROOT/holder.out"
hold_conn "$HP_R" "$HOLD_FLAG" "$HOLD_OUT"
for _ in {1..40}; do [[ -e "$HOLD_FLAG" ]] && break; sleep 0.25; done
if [[ ! -e "$HOLD_FLAG" ]]; then
  fail "holder connection never established"
else
  # Sleep MUST complete with the connection open (forced, snapshot captures the
  # guest-side ESTABLISHED entry), and the VM must be gone.
  if cli app sleep r1 >/dev/null 2>&1 && wait_phase_of r1 asleep 60 && wait_fc "$FC0" 40; then
    pass "forced sleep completed with a client connected (VM freed)"
  else
    fail "sleep did not complete with an open connection: phase=$(phase_of r1) fc=$(fc_count)"
  fi
  # The holder's socket must END (reap closes it host-side within
  # connection-idle-timeout; the snapshot-stopped guest black-holes the pipe, so
  # without the reaper this would hang) — 45s recv timeout in the holder is the
  # failure detector.
  wait "$HOLDER_PID" 2>/dev/null; HOLDER_PID=""
  case "$(cat "$HOLD_OUT" 2>/dev/null)" in
    eof|reset) pass "pre-sleep connection was closed, not preserved or wedged ($(cat "$HOLD_OUT"))" ;;
    data)      fail "pre-sleep connection received unexpected bytes after sleep" ;;
    *)         fail "pre-sleep connection black-holed (never closed within 45s)" ;;
  esac
fi

echo "== 08 the WAKING connection carries the command; pre-sleep data intact, wake in place"
# One connection: connect to the slept app and immediately send GET — the bytes
# must wait in the socket across the restore and be answered by the woken redis.
GOT="$(redis_cmd "$HP_R" 45 GET smoke_k)"
if [[ "$GOT" == *crucible-e2* ]]; then
  pass "waking connection's own command answered with the pre-sleep value"
else
  fail "GET on the waking connection returned '$GOT' (want crucible-e2)"
fi
if wait_phase_of r1 running 60 && wait_fc $((FC0+1)) 40; then
  INST_AFTER="$(inst_of r1)"
  if [[ -n "$INST_AFTER" && "$INST_AFTER" == "$INST_BEFORE" ]]; then
    pass "wake was in place (same instance $INST_AFTER)"
  else
    fail "wake was not in place: $INST_BEFORE → $INST_AFTER"
  fi
  # And the stale guest-side ESTABLISHED entry did not wedge the server.
  [[ "$(redis_cmd "$HP_R" 20 PING)" == *PONG* ]] \
    && pass "fresh connections serve after wake (stale guest conn is inert)" \
    || fail "redis wedged after wake with a stale guest-side connection"
else
  fail "app not running after the waking connection: phase=$(phase_of r1) fc=$(fc_count)"
fi

echo "== 09 a request racing sleep converges (served + exactly one VM)"
# Sleep and wake serialize on the app's transition lock: a connection landing
# mid-snapshot resolves ErrAsleep -> coalesced wake -> blocks until the sleep
# finishes -> restore -> served. Sample the window at three offsets.
RACE_OK=1
for D in 0 0.2 0.5; do
  # ensure running before each round (previous round may have ended either way)
  redis_cmd "$HP_R" 45 PING >/dev/null 2>&1
  if ! wait_phase_of r1 running 60; then RACE_OK=0; fail "race setup: app not running"; break; fi
  cli app sleep r1 >/dev/null 2>&1 &
  SLEEP_BG=$!
  sleep "$D"
  R="$(redis_cmd "$HP_R" 45 PING)"
  wait "$SLEEP_BG" 2>/dev/null
  if [[ "$R" != *PONG* ]]; then
    RACE_OK=0; fail "race (delay ${D}s): request was dropped, reply '$R'"
  fi
  # never zero-and-stuck, never two VMs
  N="$(fc_count)"
  if [[ "$N" -gt $((FC0+1)) ]]; then
    RACE_OK=0; fail "race (delay ${D}s): $((N-FC0)) VMs after the race (want <=1)"
  fi
done
if [[ "$RACE_OK" -eq 1 ]]; then
  pass "3/3 requests racing sleep were served with <=1 VM throughout"
fi

echo "== 10 wake herd: concurrent connections to a slept app all serve, one VM"
redis_cmd "$HP_R" 45 PING >/dev/null 2>&1; wait_phase_of r1 running 60
if ! cli app sleep r1 >/dev/null 2>&1 || ! wait_phase_of r1 asleep 60; then
  fail "herd setup: could not sleep the app (phase=$(phase_of r1))"
else
  for i in 1 2 3; do
    redis_cmd "$HP_R" 45 PING >"$SMOKE_ROOT/herd.$i" 2>/dev/null &
    HERD_PIDS[$i]=$!
  done
  HERD_SERVED=0
  for i in 1 2 3; do
    wait "${HERD_PIDS[$i]}" 2>/dev/null
    [[ "$(cat "$SMOKE_ROOT/herd.$i" 2>/dev/null)" == *PONG* ]] && HERD_SERVED=$((HERD_SERVED+1))
  done
  if [[ "$HERD_SERVED" -eq 3 ]] && wait_fc $((FC0+1)) 40; then
    pass "herd of 3 concurrent connections: all served by exactly one restored VM"
  else
    fail "herd: served=$HERD_SERVED/3, VMs=$(( $(fc_count) - FC0 )) (want 3 served, 1 VM)"
  fi
fi
cli app rm r1 >/dev/null 2>&1 || true

echo "==============================================================="
echo " sleep/wake correctness: $PASS passed, $FAIL failed"
echo " transcripts: $SMOKE_ROOT   (daemon log: $DAEMON_LOG)"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
