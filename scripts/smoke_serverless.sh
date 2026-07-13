#!/usr/bin/env bash
#
# smoke_serverless.sh — end-to-end check for wake-on-TCP (L4 scale-to-zero).
#
# Boots a real daemon under jailer (NO ingress proxy — the whole point is that a
# TCP service reached through a PUBLISHED host port scales to zero and wakes on
# connect, with no HTTP proxy in the path), then verifies two paths:
#
#   A. Non-volume fast path (nginx over its published port):
#      - a scale-to-zero app auto-sleeps after its idle_timeout (the L4 forwarder
#        feeds the idle monitor), freeing its VM;
#      - the published host port stays bound while asleep, and the next TCP
#        connection WAKES the app in place (snapshot restore) and forwards.
#
#   B. Volume cold-boot path (serverless postgres — the north-star):
#      - a scale-to-zero postgres on a persistent volume, published on :5432;
#      - manual `app sleep` frees the VM (stop/start for a volume app);
#      - a fresh TCP connection wakes it (cold-create + volume re-attach), and the
#        row written before sleep is still there afterward (durable across wake).
#
#   C. Request/response redis (reaping ON, the default): a client that HOLDS a
#      connection open + idle is still reaped, so the app reaches zero connections
#      and sleeps — proving scale-to-zero works for pooled clients, not just HTTP.
#
#   D. Pub/sub redis (--keep-connections): a subscriber's idle connection is NOT
#      reaped, so the app stays awake while subscribed and sleeps only once the
#      last subscriber disconnects — connection-scoped scale-to-zero for streaming.
#
# Together these show wake-on-TCP is protocol-agnostic (nginx, postgres, redis)
# and covers both request/response and connection-scoped (pub/sub) workloads.
#
# Requires: root + KVM, firecracker + jailer + vmlinux + a rootfs whose agent has
# /mount (build with the current crucible-agent), mkfs.ext4, curl, a default
# egress iface (published host ports need the network manager up), and internet
# to pull nginx:alpine + postgres:16-alpine. Postgres needs a few hundred MB and
# a first-boot initdb, so its timeouts are generous.
#
# The volume-dir sits on the SAME filesystem as the chroot base so volumes
# hardlink into the jail (a cross-fs volume-dir is rejected by design).
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_serverless.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
LISTEN="${LISTEN:-127.0.0.1:7911}"
BASE_URL="http://${LISTEN}"
MOUNT="${MOUNT:-/var/lib/crucible-serverless}"
NGINX_IMAGE="${NGINX_IMAGE:-nginx:alpine}"
PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
HP_NGINX="${HP_NGINX:-7920}"   # host port for the non-volume app
HP_PG="${HP_PG:-7921}"         # host port for the volume postgres
HP_RR="${HP_RR:-7922}"         # host port for the request/response redis
HP_PS="${HP_PS:-7923}"         # host port for the pub/sub redis
REDIS_IMAGE="${REDIS_IMAGE:-redis:alpine}"

pass=0; fail=0
ok()  { echo "  ✓ $*"; pass=$((pass+1)); }
bad() { echo "  ✗ $*"; fail=$((fail+1)); }

echo "==============================================================="
echo " crucible serverless (wake-on-TCP) smoke"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------
[[ $EUID -eq 0 ]]        || { echo "error: must run as root (KVM + jailer)" >&2; exit 2; }
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for b in "$FIRECRACKER_BIN" "$JAILER_BIN"; do [[ -x "$b" ]] || { echo "error: missing $b" >&2; exit 2; }; done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r "$ROOTFS" ]] || { echo "error: rootfs not readable: $ROOTFS" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null || { echo "error: mkfs.ext4 needed (e2fsprogs)" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }
EGRESS="${EGRESS:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS" ]] || { echo "error: no default egress iface (set EGRESS=<nic>); published ports need it" >&2; exit 2; }
if systemctl is-active --quiet crucible 2>/dev/null; then
  echo "error: systemd crucible is active — stop it first (this starts its own daemon)" >&2; exit 2
fi

# ---- work root (one fs so volumes hardlink into the jail) -------------------
echo "== 01 prepare work root ($MOUNT), egress iface $EGRESS"
rm -rf "$MOUNT"; mkdir -p "$MOUNT"/{run,jailer,volumes,images,logs}
cp "$ROOTFS" "$MOUNT/rootfs.ext4"
DAEMON_LOG="$MOUNT/daemon.log"

DAEMON_PID=""
cleanup() {
  [[ -n "$DAEMON_PID" ]] && kill -TERM "$DAEMON_PID" 2>/dev/null && wait "$DAEMON_PID" 2>/dev/null
  pkill -9 -f 'firecracker --id' 2>/dev/null || true
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$MOUNT"
}
trap cleanup EXIT

start_daemon() {
  "$CRUCIBLE_BIN" daemon --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$MOUNT/jailer" --kernel "$KERNEL" --rootfs "$MOUNT/rootfs.ext4" \
    --work-base "$MOUNT/run" --image-dir "$MOUNT/images" --log-dir "$MOUNT/logs" \
    --volume-dir "$MOUNT/volumes" --app-db "$MOUNT/apps.db" \
    --network-egress-iface "$EGRESS" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never became healthy"; tail -20 "$DAEMON_LOG"; exit 3
}

echo "== 02 start daemon (apps + volumes, NO ingress proxy)"
start_daemon
echo "   daemon healthy (pid $DAEMON_PID)"

run() { "$CRUCIBLE_BIN" --addr "$BASE_URL" "$@"; }

# ---- helpers ----------------------------------------------------------------
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
# wait_phase APP WANT [TRIES]
wait_phase() { local want="$2" tries="${3:-120}"; for _ in $(seq 1 "$tries"); do [[ "$(app_phase "$1")" == "$want" ]] && return 0; sleep 0.5; done; return 1; }
fc_count() { pgrep -f 'firecracker --id' 2>/dev/null | wc -l | tr -d ' '; }
# wait_fc WANT [TRIES]
wait_fc() { local want="$1" tries="${2:-60}"; for _ in $(seq 1 "$tries"); do [[ "$(fc_count)" -eq "$want" ]] && return 0; sleep 0.5; done; return 1; }
# http_get URL NEEDLE — curl until the body contains needle (wakes a slept app).
http_get() { local url="$1" needle="$2"; for _ in $(seq 1 40); do [[ "$(curl -s --max-time 10 "$url" 2>/dev/null)" == *"$needle"* ]] && return 0; sleep 0.5; done; return 1; }
# tcp_poke PORT — open a raw TCP connection (triggers wake-on-connect), hold, close.
tcp_poke() { timeout 5 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1; sleep 2" 2>/dev/null || true; }
# port_open PORT — 0 if something accepts a TCP connection on the host port.
port_open() { timeout 2 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1" 2>/dev/null; }
# pg_query SQL — run a query in the pg app's current instance over TCP loopback
# (the endpoint the tcp:5432 health check validates), password-authenticated via
# PGPASSWORD, returning just the scalar result. `env VAR=val cmd` avoids sh -c.
pg_query() { run app exec pg -- env PGPASSWORD=smoke psql -h 127.0.0.1 -U postgres -tAc "$1" 2>/dev/null | tr -d '\r\n '; }
# redis_ping PORT — open a raw TCP connection to the published redis port, send an
# inline PING, and return 0 if the reply contains PONG (proving the L4 forwarder
# reached a live redis). Connecting also wakes a slept app.
redis_ping() {
  local reply
  exec 3<>"/dev/tcp/127.0.0.1/$1" 2>/dev/null || return 1
  printf 'PING\r\n' >&3
  IFS= read -t 20 -r reply <&3
  exec 3<&- 2>/dev/null || true
  [[ "$reply" == *PONG* ]]
}

FC0="$(fc_count)"   # firecracker procs before this smoke (usually 0)

# =============================================================================
# A. Non-volume fast path: nginx auto-sleeps then wakes on the next connection.
# =============================================================================
echo "== 03 scale-to-zero nginx over a published port: boot + serve"
run app create web --image "$NGINX_IMAGE" -p "$HP_NGINX:80" --restart always \
  --health "tcp:80" --memory 256 --min-scale 0 --idle-timeout 5s >/dev/null 2>&1
if wait_phase web running 240 && wait_fc $((FC0+1)) 60; then
  ok "scale-to-zero app booted (1 VM), host port :$HP_NGINX owned by the L4 forwarder"
  if http_get "http://127.0.0.1:$HP_NGINX/" "nginx" || http_get "http://127.0.0.1:$HP_NGINX/" "html"; then
    ok "L4 forwarder forwards the published port to the running instance (no proxy)"
  else
    bad "published port :$HP_NGINX did not serve"
  fi

  echo "== 04 auto idle-sleep frees the VM; host port stays bound"
  # Nothing connects for > idle_timeout (5s); the idle monitor sleeps it.
  if wait_phase web asleep 60 && wait_fc "$FC0" 40; then
    ok "idle → phase=asleep, VM freed (scale-to-zero on TCP inactivity)"
  else
    bad "app did not idle-sleep: phase=$(app_phase web) fc=$(fc_count) want $FC0"
  fi

  echo "== 05 next TCP connection wakes it in place and serves again"
  if http_get "http://127.0.0.1:$HP_NGINX/" "nginx" || http_get "http://127.0.0.1:$HP_NGINX/" "html"; then
    if wait_phase web running 60 && wait_fc $((FC0+1)) 40; then
      ok "wake-on-connect: snapshot-restored + served, exactly one VM back"
    else
      bad "woke but VM count wrong: phase=$(app_phase web) fc=$(fc_count)"
    fi
  else
    bad "connection to a slept app did not wake + serve it"
  fi

  run app rm web >/dev/null 2>&1
  wait_fc "$FC0" 40 >/dev/null || true
  # The forwarder is closed by the reconcile pass that drops the app — a hair
  # after the VM teardown wait_fc observes — so poll until the port stops
  # accepting rather than probing once and racing the close.
  freed=0
  for _ in $(seq 1 20); do
    port_open "$HP_NGINX" || { freed=1; break; }
    sleep 0.5
  done
  [[ "$freed" -eq 1 ]] && ok "app rm freed the host port :$HP_NGINX (forwarder closed)" \
    || bad "host port :$HP_NGINX still accepts 10s after app rm (forwarder leaked)"
else
  bad "scale-to-zero nginx never reached running (is $NGINX_IMAGE pullable? fc=$(fc_count)): $(run app get web 2>/dev/null | head -c 300)"
  run app rm web >/dev/null 2>&1 || true
fi

# =============================================================================
# B. Volume cold-boot path: serverless postgres — sleep, wake-on-connect, data.
# =============================================================================
echo "== 06 serverless postgres on a volume: boot (first-boot initdb)"
# PGDATA is a SUBDIR of the mount so the volume's lost+found doesn't make postgres
# think the data dir is already initialized (the classic ext4-volume gotcha).
run app create pg --image "$PG_IMAGE" -p "$HP_PG:5432" --restart always \
  --health "tcp:5432" --memory 512 --min-scale 0 --idle-timeout 3600s \
  --volume pgdata:/var/lib/postgresql/data \
  -e POSTGRES_PASSWORD=smoke \
  -e PGDATA=/var/lib/postgresql/data/pgdata >/dev/null 2>&1
  # Password-authenticated (the normal way): the postgres entrypoint feeds initdb
  # a pwfile via bash process substitution (--pwfile=<(...) → /dev/fd/N). That now
  # works because the guest init provides /dev/fd → /proc/self/fd; clients then
  # authenticate over TCP with PGPASSWORD.
if wait_phase pg running 400 && wait_fc $((FC0+1)) 60; then
  ok "postgres booted with its volume (1 VM), :$HP_PG fronted by the L4 forwarder"

  echo "== 07 write a row (wait for postgres to accept queries)"
  ready=0
  for _ in $(seq 1 90); do
    [[ "$(run app exec pg -- pg_isready -h 127.0.0.1 2>/dev/null)" == *"accepting connections"* ]] && { ready=1; break; }
    sleep 1
  done
  if [[ "$ready" -ne 1 ]]; then
    bad "postgres never became ready"
    echo "     --- app get pg ---"
    run app get pg 2>&1 | head -c 600; echo
    echo "     --- postgres service log (tail) ---"
    run app logs pg --source service 2>&1 | tail -25
  else
    cerr="$(run app exec pg -- env PGPASSWORD=smoke psql -h 127.0.0.1 -U postgres -v ON_ERROR_STOP=1 \
      -c 'CREATE TABLE IF NOT EXISTS t(x int)' -c 'INSERT INTO t VALUES (4242)' 2>&1)"
    if [[ "$(pg_query 'SELECT x FROM t')" == "4242" ]]; then
      ok "wrote row (x=4242) via password-authenticated psql over TCP"
    else
      bad "write failed: ${cerr:0:200}"
    fi
  fi

  echo "== 08 manual sleep destroys the instance (stop/start, volume detached)"
  run app sleep pg >/dev/null 2>&1
  if wait_phase pg asleep 60 && wait_fc "$FC0" 40; then
    ok "app sleep → phase=asleep, VM freed"
  else
    bad "postgres did not sleep: phase=$(app_phase pg) fc=$(fc_count)"
  fi

  echo "== 09 a fresh TCP connection cold-wakes it; the row survives"
  tcp_poke "$HP_PG"   # connect to the published port → triggers wake-on-connect
  if wait_phase pg running 400 && wait_fc $((FC0+1)) 60; then
    ok "wake-on-connect cold-created a fresh instance (volume re-attached)"
    got=""
    for _ in $(seq 1 60); do
      got="$(pg_query 'SELECT x FROM t')"
      [[ "$got" == "4242" ]] && break
      sleep 1
    done
    [[ "$got" == "4242" ]] && ok "row survived sleep→wake on the volume (durable serverless postgres)" \
      || bad "expected x=4242 after wake, got '$got'"
  else
    bad "connection did not wake postgres: phase=$(app_phase pg) fc=$(fc_count): $(run app get pg 2>/dev/null | head -c 300)"
  fi

  run app rm pg >/dev/null 2>&1 || true
  wait_fc "$FC0" 40 >/dev/null || true
  run volume rm pgdata >/dev/null 2>&1 || true
else
  bad "postgres never reached running (is $PG_IMAGE pullable? enough RAM? fc=$(fc_count)): $(run app get pg 2>/dev/null | head -c 400)"
  run app rm pg >/dev/null 2>&1 || true
  run volume rm pgdata >/dev/null 2>&1 || true
fi

# =============================================================================
# C. Request/response redis (reap on): a held-idle pooled connection is reaped so
#    the app still scales to zero — proving it's not just postgres.
# =============================================================================
echo "== 10 request/response redis (reap on): a held-idle connection still lets it sleep"
# default --connection-idle-timeout = --idle-timeout (5s), so a silent connection
# is reaped ~5s after it goes quiet, then the app sleeps ~5s later.
run app create rr --image "$REDIS_IMAGE" -p "$HP_RR:6379" --restart always \
  --health "tcp:6379" --memory 256 --min-scale 0 --idle-timeout 5s >/dev/null 2>&1
if wait_phase rr running 240 && wait_fc $((FC0+1)) 60; then
  redis_ping "$HP_RR" && ok "redis served PING/PONG over the L4 forwarder" \
    || bad "redis did not answer PING on :$HP_RR"
  # Hold a connection open and idle (a pooled client). Reaping must still let the
  # app reach zero connections and sleep.
  ( exec 3<>"/dev/tcp/127.0.0.1/$HP_RR"; printf 'PING\r\n' >&3; IFS= read -t 5 -r _ <&3; sleep 60 ) &
  HOLDER=$!
  if wait_phase rr asleep 60 && wait_fc "$FC0" 20; then
    ok "idle connection reaped → app slept despite a held-open client (VM freed)"
  else
    bad "reap-on app did not sleep with a held connection: phase=$(app_phase rr) fc=$(fc_count)"
  fi
  kill "$HOLDER" 2>/dev/null || true
  # A fresh connection wakes it again.
  redis_ping "$HP_RR" >/dev/null 2>&1
  wait_phase rr running 60 && wait_fc $((FC0+1)) 30 \
    && ok "wake-on-connect brought redis back" \
    || bad "redis did not wake on reconnect: phase=$(app_phase rr) fc=$(fc_count)"
  run app rm rr >/dev/null 2>&1; wait_fc "$FC0" 20 >/dev/null || true
else
  bad "request/response redis never reached running (is $REDIS_IMAGE pullable? fc=$(fc_count)): $(run app get rr 2>/dev/null | head -c 300)"
  run app rm rr >/dev/null 2>&1 || true
fi

# =============================================================================
# D. Pub/sub redis (keep-connections): a subscriber holds the app AWAKE, and it
#    sleeps only once the subscriber disconnects — connection-scoped scale-to-zero.
# =============================================================================
echo "== 11 pub/sub redis (--keep-connections): awake while subscribed, sleeps on disconnect"
run app create ps --image "$REDIS_IMAGE" -p "$HP_PS:6379" --restart always \
  --health "tcp:6379" --memory 256 --min-scale 0 --idle-timeout 5s --keep-connections >/dev/null 2>&1
if wait_phase ps running 240 && wait_fc $((FC0+1)) 60; then
  redis_ping "$HP_PS" && ok "pub/sub redis served PING/PONG over the L4 forwarder" \
    || bad "pub/sub redis did not answer PING on :$HP_PS"
  # A subscriber holds a (byte-idle) connection. With reaping OFF it must NOT be
  # closed, so the app stays awake well past its idle_timeout.
  ( exec 3<>"/dev/tcp/127.0.0.1/$HP_PS"; printf 'SUBSCRIBE ch\r\n' >&3; sleep 60 ) &
  SUB=$!
  sleep 18   # > 3× idle_timeout: a reap-on app would have slept by now
  if [[ "$(app_phase ps)" == "running" ]] && [[ "$(fc_count)" -eq $((FC0+1)) ]]; then
    ok "keep-connections held the app awake with an idle subscriber (no reap)"
  else
    bad "pub/sub app slept out from under a subscriber: phase=$(app_phase ps) fc=$(fc_count)"
  fi
  # Drop the subscriber → zero connections → the app sleeps after idle_timeout.
  kill "$SUB" 2>/dev/null || true
  if wait_phase ps asleep 40 && wait_fc "$FC0" 20; then
    ok "slept after the last subscriber disconnected (connection-scoped scale-to-zero)"
  else
    bad "pub/sub app did not sleep after the subscriber left: phase=$(app_phase ps) fc=$(fc_count)"
  fi
  redis_ping "$HP_PS" >/dev/null 2>&1
  wait_phase ps running 60 && wait_fc $((FC0+1)) 30 \
    && ok "wake-on-connect brought the pub/sub app back" \
    || bad "pub/sub redis did not wake on reconnect: phase=$(app_phase ps)"
  run app rm ps >/dev/null 2>&1; wait_fc "$FC0" 20 >/dev/null || true
else
  bad "pub/sub redis never reached running (fc=$(fc_count)): $(run app get ps 2>/dev/null | head -c 300)"
  run app rm ps >/dev/null 2>&1 || true
fi

echo "==============================================================="
echo " serverless smoke: $pass passed, $fail failed"
echo "==============================================================="
[[ $fail -eq 0 ]]
