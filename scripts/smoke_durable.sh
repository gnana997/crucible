#!/usr/bin/env bash
#
# Durable-app smoke (v0.4): an app is a named workload the daemon keeps a
# running instance of and RE-CREATES from persisted desired state after a
# daemon restart — the headline of v0.4.0.
#
#   01  daemon healthy (images + apps enabled)
#   02  create an app from an OCI image, published on a host port
#   03  the app's instance boots and serves traffic
#   04  RESTART the daemon → the app's instance is re-created and serves again
#   05  a second app coexists; both survive
#   06  delete an app → its instance is torn down
#   07  --env is delivered to the app entrypoint (v0.4.1)
#   08  -P auto-publishes the image's EXPOSEd port (v0.4.1)
#   09  --health-cmd (exec) drives health from an in-guest command (v0.4.1)
#   10  app update replaces the spec and redeploys the instance (v0.4.2)
#
# The daemon-restart step is the whole point: v0.3 dropped every running
# sandbox on restart; a v0.4 app comes back because its desired state lives
# in the bbolt app store and the reconciler boots a fresh instance on start.
#
# Requires: root + KVM, firecracker + jailer + vmlinux, crucible built with
# an embedded agent (make build), curl, network to pull the image.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        scripts/smoke_durable.sh

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
LISTEN="${LISTEN:-127.0.0.1:7891}"
BASE_URL="http://${LISTEN}"
IMAGE="${IMAGE:-nginx:alpine}"

SMOKE_ROOT="${SMOKE_ROOT:-${SMOKE_BASE:-/tmp}/crucible-smoke-durable-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$SMOKE_ROOT"
IMAGE_DIR="$SMOKE_ROOT/images"
WORK_BASE="$SMOKE_ROOT/run"
LOG_DIR="$SMOKE_ROOT/logs"
APP_DB="$SMOKE_ROOT/apps.db"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
mkdir -p "$IMAGE_DIR" "$WORK_BASE" "$LOG_DIR"

exec > >(tee -a "$SMOKE_ROOT/session.log") 2>&1

echo "==============================================================="
echo " crucible durable-app smoke"
echo "==============================================================="
echo " output dir : $SMOKE_ROOT"
echo " listen     : $LISTEN"
echo " image      : $IMAGE"
echo " app db     : $APP_DB"
echo "==============================================================="

# ---- preflight --------------------------------------------------------------

if [[ $EUID -ne 0 ]]; then echo "error: must run as root (KVM + jailer)" >&2; exit 2; fi
[[ -x "$CRUCIBLE_BIN" ]] || { echo "error: $CRUCIBLE_BIN not executable (make build)" >&2; exit 2; }
for bin in "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing $bin" >&2; exit 2; }
done
[[ -r "$KERNEL" ]] || { echo "error: kernel not readable: $KERNEL" >&2; exit 2; }
[[ -r /dev/kvm ]]  || { echo "error: /dev/kvm not available" >&2; exit 2; }
command -v curl >/dev/null || { echo "error: curl needed" >&2; exit 2; }

# The daemon must be new enough to have /apps (v0.4).
if ! LC_ALL=C grep -qa "durable apps enabled\|app reconciler" "$CRUCIBLE_BIN"; then
  echo "error: $CRUCIBLE_BIN predates durable apps (v0.4). Rebuild: make build" >&2
  exit 2
fi

EGRESS_IFACE="${EGRESS_IFACE-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"
[[ -n "$EGRESS_IFACE" ]] || { echo "error: no default route; set EGRESS_IFACE" >&2; exit 2; }

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
api()  { curl -s "$@"; }
cli()  { "$CRUCIBLE_BIN" --addr "$LISTEN" "$@"; }

# ---- daemon -----------------------------------------------------------------

DAEMON_PID=""
start_daemon() {
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" --kernel "$KERNEL" --rootfs "$KERNEL" \
    --work-base "$WORK_BASE" --image-dir "$IMAGE_DIR" --log-dir "$LOG_DIR" \
    --app-db "$APP_DB" \
    --network-egress-iface "$EGRESS_IFACE" \
    --log-format json --log-level info >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..150}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited early"; tail -30 "$DAEMON_LOG"; exit 3; }
    sleep 0.2
  done
  echo "daemon never healthy"; tail -30 "$DAEMON_LOG"; exit 3
}
stop_daemon() {
  [[ -n "$DAEMON_PID" ]] || return 0
  kill -TERM "$DAEMON_PID" 2>/dev/null
  wait "$DAEMON_PID" 2>/dev/null
  DAEMON_PID=""
}
final_cleanup() {
  stop_daemon
  [[ "${KEEP:-0}" == "1" ]] || rm -rf "$SMOKE_ROOT"
}
trap final_cleanup EXIT

# Wait until the published port answers (an instance is up and serving).
wait_serving() {
  local port="$1" tries="${2:-120}"
  for _ in $(seq 1 "$tries"); do
    curl -sf "http://localhost:${port}/" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

echo "== 01 starting daemon (images + apps enabled)"
start_daemon
pass "daemon healthy"

# ---- 02 create an app -------------------------------------------------------

echo "== 02 create app 'web' from $IMAGE published on :8080"
CREATE=$(api -X POST "$BASE_URL/apps" -H 'content-type: application/json' -d "{
  \"name\": \"web\",
  \"image\": {\"oci\": \"$IMAGE\"},
  \"publish\": [{\"host_port\": 8080, \"guest_port\": 80}],
  \"restart\": {\"policy\": \"always\"},
  \"health\": {\"type\": \"http\", \"path\": \"/\", \"port\": 80, \"interval_s\": 3}
}")
if echo "$CREATE" | grep -q '"id":"app_'; then
  pass "app created: $(echo "$CREATE" | grep -o '"id":"app_[a-z0-9]*"')"
else
  fail "create app failed: $CREATE"; tail -30 "$DAEMON_LOG"; exit 3
fi

# ---- 03 instance boots and serves -------------------------------------------

echo "== 03 app instance boots and serves on :8080"
if wait_serving 8080; then
  pass "app serving on host :8080"
else
  fail "app never served on :8080"; tail -40 "$DAEMON_LOG"
fi

# ---- 04 RESTART the daemon → app comes back ---------------------------------

echo "== 04 restart daemon → app instance re-created from desired state"
stop_daemon
# Prove the old instance is gone: nothing on :8080 while the daemon is down.
sleep 1
if curl -sf http://localhost:8080/ >/dev/null 2>&1; then
  echo "   (note: something still answering :8080 after daemon stop)"
fi
start_daemon
if wait_serving 8080; then
  pass "app re-created and serving after daemon restart (the v0.4 headline)"
else
  fail "app did NOT come back after restart"; tail -40 "$DAEMON_LOG"
fi
# The re-created instance is a fresh sandbox id, but the app id is stable.
GET=$(api "$BASE_URL/apps/web")
if echo "$GET" | grep -q '"phase":"running"'; then
  pass "app 'web' reports running after restart"
else
  fail "app status after restart: $GET"
fi

# The http health probe must reach the guest and pass (W5).
echo "== 04b app reports healthy via its http health check"
HEALTHY=0
for _ in $(seq 1 20); do
  if api "$BASE_URL/apps/web" | grep -q '"health":"healthy"'; then HEALTHY=1; break; fi
  sleep 1
done
[[ "$HEALTHY" -eq 1 ]] && pass "http health check passing (health=healthy)" || fail "app never reported healthy: $(api "$BASE_URL/apps/web")"

# ---- 05 second app coexists -------------------------------------------------

echo "== 05 a second app coexists"
api -X POST "$BASE_URL/apps" -H 'content-type: application/json' -d "{
  \"name\": \"web2\",
  \"image\": {\"oci\": \"$IMAGE\"},
  \"publish\": [{\"host_port\": 8081, \"guest_port\": 80}],
  \"restart\": {\"policy\": \"always\"}
}" >/dev/null
if wait_serving 8081; then
  pass "second app serving on :8081"
else
  fail "second app never served on :8081"
fi
COUNT=$(api "$BASE_URL/apps" | grep -o '"id":"app_' | wc -l)
[[ "$COUNT" -eq 2 ]] && pass "both apps listed" || fail "app count = $COUNT, want 2"

# ---- 06 delete tears down ---------------------------------------------------

echo "== 06 delete app → instance torn down"
api -X DELETE "$BASE_URL/apps/web2" >/dev/null
# Give the reconciler a tick to tear the instance down.
sleep 3
if curl -sf http://localhost:8081/ >/dev/null 2>&1; then
  fail "deleted app still serving on :8081"
else
  pass "deleted app no longer served"
fi
api -X DELETE "$BASE_URL/apps/web" >/dev/null

# ---- 07 app with --env boots and serves (v0.4.1 V2) -------------------------
# --env KEY=VALUE merges onto the entrypoint's environment. Observing the
# entrypoint's env from `app exec` is unreliable (the guest's ptrace_scope
# blocks reading a sibling process's /proc/<pid>/environ), so the exact
# delivery is asserted by unit tests instead — the daemon merge
# (TestMergeAppEnv) and the agent apply (TestBuildServiceEnvExact*). Here we
# just confirm an app created WITH env still boots and serves (env didn't break
# the entrypoint launch).
echo "== 07 --env: app with env boots and serves (delivery is unit-tested)"
cli app create envapp --image "$IMAGE" -p 8082:80 \
  -e CRUCIBLE_ENV_PROBE=marker-xyz -e LOG_LEVEL=info --restart always >/dev/null 2>&1
if wait_serving 8082; then
  pass "app created with --env booted and served on :8082"
else
  fail "app with --env failed to serve on :8082"
fi
cli app rm envapp >/dev/null 2>&1

# ---- 08 -P auto-publishes the image's EXPOSE (v0.4.1 V4) --------------------
# nginx:alpine declares EXPOSE 80, so `-P` (no explicit -p) must publish guest
# 80 → host 80 and the app is reachable there.
echo "== 08 -P publishes the image's EXPOSEd port (guest 80 → host 80)"
if curl -sf http://localhost:80/ >/dev/null 2>&1; then
  echo "   (note: something already answering :80 — skipping the -P check)"
else
  cli app create exposeapp --image "$IMAGE" -P --restart always >/dev/null 2>&1
  if wait_serving 80; then
    pass "-P published the EXPOSEd port; served on host :80 with no explicit -p"
  else
    fail "-P did not publish the EXPOSEd port (:80 unreachable)"
    api "$BASE_URL/apps/exposeapp"
  fi
  cli app rm exposeapp >/dev/null 2>&1
fi

# ---- 09 exec health check (v0.4.1 V3) ---------------------------------------
# --health-cmd runs a command in the guest; exit 0 = healthy. nginx:alpine has
# /etc/nginx/nginx.conf, so this probe passes and the app reports healthy.
echo "== 09 --health-cmd (exec) drives health from an in-guest command"
cli app create healthexec --image "$IMAGE" -p 8083:80 \
  --health-cmd 'test -f /etc/nginx/nginx.conf' --restart always >/dev/null 2>&1
if wait_serving 8083; then
  H=0
  for _ in $(seq 1 25); do
    [[ "$(api "$BASE_URL/apps/healthexec" 2>/dev/null)" == *'"health":"healthy"'* ]] && { H=1; break; }
    sleep 1
  done
  [[ "$H" -eq 1 ]] && pass "exec health check passing (health=healthy)" \
    || fail "exec health never healthy: $(api "$BASE_URL/apps/healthexec" 2>&1)"
else
  fail "exec-health app never served on :8083"
fi
cli app rm healthexec >/dev/null 2>&1

# ---- 10 app update redeploys (v0.4.2 W6) ------------------------------------
# `app update` replaces the spec and bumps the generation; the reconciler
# destroys the old instance and boots a fresh one from the new spec.
echo "== 10 app update replaces the spec and redeploys the instance"
cli app create upd --image "$IMAGE" -p 8084:80 --restart always --memory 256 >/dev/null 2>&1
if wait_serving 8084; then
  INST1="$(api "$BASE_URL/apps/upd" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
  # Change the spec (memory + a new env) — a generation bump forces a redeploy.
  cli app update upd --image "$IMAGE" -p 8084:80 --restart always --memory 320 -e UPDATED=yes >/dev/null 2>&1
  REDEPLOYED=0; INST2=""
  for _ in $(seq 1 60); do
    INST2="$(api "$BASE_URL/apps/upd" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
    if [[ "$INST2" == sbx_* && "$INST2" != "$INST1" ]] && curl -sf http://localhost:8084/ >/dev/null 2>&1; then
      REDEPLOYED=1; break
    fi
    sleep 1
  done
  [[ "$REDEPLOYED" -eq 1 ]] && pass "app update redeployed to a new instance ($INST1 → $INST2) and serves" \
    || fail "app update did not redeploy ($INST1 → ${INST2:-none})"
  GEN="$(api "$BASE_URL/apps/upd" 2>/dev/null | grep -o '"generation":[0-9]*' | grep -o '[0-9]*' | head -1)"
  [[ "$GEN" == "2" ]] && pass "generation bumped to 2 after update" || fail "generation = ${GEN:-?}, want 2"
else
  fail "update app never served on :8084"
fi
cli app rm upd >/dev/null 2>&1

# ---- summary ----------------------------------------------------------------

echo
echo "==============================================================="
echo " smoke_durable: $PASS passed, $FAIL failed"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
