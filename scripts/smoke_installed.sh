#!/usr/bin/env bash
#
# Release-acceptance smoke against the ALREADY-RUNNING installed daemon.
#
# Unlike the other smokes (which spin up their own daemon), this drives the
# `crucible` you'd get from a real install — the binary on your PATH talking to
# the systemd-managed daemon at 127.0.0.1:7878 — through the full user journey:
# run/shell/exec, egress deny, logs, snapshot+fork, --disk, stop/rm, build,
# durable apps (v0.4), the MCP server, the full v0.4.1 surface (app --env, exec
# health, full-egress + its SSRF tripwire, --net-allow-cidr, and -P), the
# v0.4.2 surface (app update, image-HEALTHCHECK seeding, and — opt-in — the
# ingress proxy), the v0.4.3 surface (operate an app by name — app exec /
# app logs — plus, through the proxy, a zero-downtime rolling update), and the
# v0.5 surface (scale-to-zero `app sleep`/`wake`, opt-in app→app networking, and
# horizontal scale-out via `--min-scale N`). It answers one question: "will
# someone who installs the release hit a wall?"
#
# Safe by construction:
#   - runs UNPRIVILEGED (the CLI is just a client; the root daemon does the work)
#   - creates its own sandboxes/snapshots/image/app and deletes ONLY those on exit
#   - never lists-and-deletes, never stops or restarts the daemon (so the durable
#     app is tested via self-heal — killing its instance — not a daemon restart)
#
# Prereqs: the daemon is running with image + durable logs enabled
#   (--image-dir and --log-dir in CRUCIBLE_FLAGS) and a default egress iface;
#   for the apps step also --app-db; internet to pull the public images; curl;
#   python3 (only for the MCP step); docker (only for the build step).
#
# Usage:
#   sudo systemctl start crucible        # make sure it's up
#   scripts/smoke_installed.sh           # no sudo needed
#
# To also exercise the opt-in ingress-proxy step (16), point it at the daemon's
# proxy listener. With install.sh's DEFAULT proxy values (--proxy-listen :7879
# --proxy-domain apps.local):
#
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux ROOTFS=./assets/rootfs-with-agent.ext4 \
#        PROXY_ADDR=127.0.0.1:7879 PROXY_DOMAIN=apps.local \
#        scripts/smoke_installed.sh
#
#   Only PROXY_ADDR + PROXY_DOMAIN matter to THIS smoke (it's a client that drives
#   the already-running daemon); the FIRECRACKER_BIN/JAILER_BIN/KERNEL/ROOTFS vars
#   are what the *daemon* needs and are inert here — harmless to leave in so the
#   same line works whether you're launching a daemon or not. Match whatever port
#   the daemon's --proxy-listen actually binds. IMPORTANT: don't point the proxy
#   at a port this smoke publishes to (:80 for -P, and HOST_PORT_A..E = 8080-8084)
#   or the two will fight over the host port — :7879 is chosen to avoid both. If
#   the daemon runs the proxy on :80, step 14 (-P → host :80) self-skips (expected).
#
# Overrides: CRUCIBLE_BIN (default: crucible on PATH), CRUCIBLE_ADDR
#   (default 127.0.0.1:7878), HOST_PORT_A..E. The ingress-proxy step is opt-in:
#   set PROXY_ADDR (e.g. 127.0.0.1:7879, wherever the daemon's --proxy-listen
#   binds) and PROXY_DOMAIN (its --proxy-domain) to exercise reach-by-name;
#   without them it's skipped, since the proxy is off by default.

set -u
set -o pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-crucible}"
ADDR="${CRUCIBLE_ADDR:-127.0.0.1:7878}"
BASE_URL="http://${ADDR}"
HERE="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-nginx:alpine}"
ALPINE="${ALPINE:-alpine:latest}"
HOST_PORT_A="${HOST_PORT_A:-8080}"
HOST_PORT_B="${HOST_PORT_B:-8081}"
HOST_PORT_C="${HOST_PORT_C:-8082}"   # durable-app instance
HOST_PORT_D="${HOST_PORT_D:-8083}"   # v0.4.1 app (env + exec health)
HOST_PORT_E="${HOST_PORT_E:-8084}"   # v0.4.2 app update
HOST_PORT_F="${HOST_PORT_F:-8085}"   # v0.5.0 sleep/wake app

echo "==============================================================="
echo " crucible installed-release acceptance smoke"
echo "==============================================================="
echo " binary : $CRUCIBLE_BIN  ($(command -v "$CRUCIBLE_BIN" 2>/dev/null || echo 'not on PATH'))"
echo " daemon : $BASE_URL"
echo "==============================================================="

command -v "$CRUCIBLE_BIN" >/dev/null 2>&1 || { echo "error: $CRUCIBLE_BIN not on PATH" >&2; exit 2; }
command -v curl >/dev/null 2>&1 || { echo "error: curl needed" >&2; exit 2; }

PASS=0; FAIL=0; SKIP=0
pass() { PASS=$((PASS+1)); echo "   PASS: $*"; }
fail() { FAIL=$((FAIL+1)); echo "   FAIL: $*"; }
skip() { SKIP=$((SKIP+1)); echo "   SKIP: $*"; }
cli()  { "$CRUCIBLE_BIN" --addr "$ADDR" "$@"; }

# --- own-resource tracking + safe cleanup ------------------------------------
CREATED_SBX=(); CREATED_SNAP=(); CREATED_IMG=(); CREATED_APP=()
track_sbx()  { CREATED_SBX+=("$1"); }
track_snap() { CREATED_SNAP+=("$1"); }
track_img()  { CREATED_IMG+=("$1"); }
track_app()  { CREATED_APP+=("$1"); }

cleanup() {
  echo "== cleanup (only what this smoke created)"
  for name in "${CREATED_APP[@]:-}"; do [[ -n "$name" ]] && cli app rm "$name" >/dev/null 2>&1 || true; done
  for id in "${CREATED_SBX[@]:-}"; do [[ -n "$id" ]] && cli sandbox rm "$id" >/dev/null 2>&1 || true; done
  for id in "${CREATED_SNAP[@]:-}"; do [[ -n "$id" ]] && cli snapshot rm "$id" >/dev/null 2>&1 || true; done
  for id in "${CREATED_IMG[@]:-}"; do [[ -n "$id" ]] && cli image rm "$id" >/dev/null 2>&1 || true; done
  [[ -n "${MCP_TMP:-}" ]] && rm -rf "$MCP_TMP" 2>/dev/null || true
}
trap cleanup EXIT

# hit <url> <needle> — curl until the body contains needle (or give up).
hit() {
  local url="$1" needle="$2" body
  for _ in {1..30}; do
    body="$(curl -s --max-time 3 "$url" 2>/dev/null || true)"
    [[ "$body" == *"$needle"* ]] && return 0
    sleep 0.5
  done
  return 1
}

# ---- 00 preflight: daemon up + version --------------------------------------
echo "== 00 daemon reachable and on v0.3.x/v0.4.x/v0.5.x"
if ! curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
  echo "error: no daemon at $BASE_URL — start it: sudo systemctl start crucible" >&2
  exit 3
fi
VER="$(cli version 2>&1)"
if [[ "$VER" == *"v0.5."* || "$VER" == *"v0.4."* || "$VER" == *"v0.3."* ]]; then pass "daemon healthy, CLI is $VER"
else fail "unexpected version: $VER (want v0.3.x, v0.4.x, or v0.5.x)"; fi

# ---- 01 boot an image + publish a port + reach it from the host -------------
echo "== 01 run $IMAGE -p $HOST_PORT_A:80 (long-lived) and curl it"
SBX="$(cli run "$IMAGE" -p "$HOST_PORT_A:80" 2>/dev/null)"
if [[ "$SBX" == sbx_* ]]; then
  track_sbx "$SBX"
  if hit "http://localhost:$HOST_PORT_A/" "html" || hit "http://localhost:$HOST_PORT_A/" "nginx"; then
    pass "booted $SBX and served on :$HOST_PORT_A (ingress works)"
  else
    fail "published port :$HOST_PORT_A unreachable"
  fi
else
  fail "run $IMAGE failed: $SBX  (does the daemon have --image-dir set?)"
fi

# ---- 02 interactive shell with persistent state -----------------------------
echo "== 02 interactive shell (cd/env persist)"
if [[ -n "${SBX:-}" && "$SBX" == sbx_* ]]; then
  OUT="$(printf 'cd /tmp && pwd\necho hi-from-shell\nexit\n' | cli shell "$SBX" 2>&1)"
  if [[ "$OUT" == *"/tmp"* && "$OUT" == *"hi-from-shell"* ]]; then
    pass "shell round-trip + shared cd state"
  else
    fail "shell output unexpected: $OUT"
  fi
else skip "no sandbox from step 01"; fi

# ---- 03 one-shot exec -------------------------------------------------------
echo "== 03 one-shot exec"
if [[ -n "${SBX:-}" && "$SBX" == sbx_* ]]; then
  if OUT="$(cli sandbox exec "$SBX" -- /bin/echo one-shot-ok 2>&1)" && [[ "$OUT" == *"one-shot-ok"* ]]; then
    pass "one-shot exec streamed output"
  else fail "one-shot exec: $OUT"; fi
else skip "no sandbox"; fi

# ---- 04 default-deny egress -------------------------------------------------
echo "== 04 egress is denied by default (no --net-allow)"
if [[ -n "${SBX:-}" && "$SBX" == sbx_* ]]; then
  if cli sandbox exec "$SBX" -- sh -c 'wget -T 3 -q -O /dev/null http://1.1.1.1/' >/dev/null 2>&1; then
    fail "egress reached 1.1.1.1 — default-deny NOT enforced!"
  else
    pass "egress to a non-allowlisted host was blocked"
  fi
else skip "no sandbox"; fi

# ---- 05 durable logs --------------------------------------------------------
echo "== 05 durable logs capture the exec activity"
if [[ -n "${SBX:-}" && "$SBX" == sbx_* ]]; then
  sleep 1
  LOGS="$(cli logs "$SBX" --source exec 2>&1 || true)"
  if [[ "$LOGS" == *"one-shot-ok"* || "$LOGS" == *"hi-from-shell"* || "$LOGS" == *"exec"* ]]; then
    pass "logs --source exec shows what ran"
  else
    fail "durable logs missing/empty: $LOGS  (does the daemon have --log-dir set?)"
  fi
else skip "no sandbox"; fi

# ---- 06 snapshot + fork -----------------------------------------------------
echo "== 06 snapshot + fork x2"
if [[ -n "${SBX:-}" && "$SBX" == sbx_* ]]; then
  SNAP="$(cli snapshot create "$SBX" 2>/dev/null)"
  if [[ "$SNAP" == snap_* ]]; then
    track_snap "$SNAP"
    FORKS="$(cli fork "$SNAP" --count 2 2>/dev/null)"
    NF=0
    while read -r fid; do [[ "$fid" == sbx_* ]] && { track_sbx "$fid"; NF=$((NF+1)); }; done <<< "$FORKS"
    if [[ "$NF" -eq 2 ]]; then pass "snapshot $SNAP → forked 2 independent sandboxes"
    else fail "fork produced $NF sandboxes, want 2: $FORKS"; fi
  else fail "snapshot create failed: $SNAP"; fi
else skip "no sandbox"; fi

# ---- 07 --disk grows the writable rootfs ------------------------------------
echo "== 07 --disk 2G grows the rootfs clone"
SBXD="$(cli run "$ALPINE" --disk 2G --memory 256 2>/dev/null)"
if [[ "$SBXD" == sbx_* ]]; then
  track_sbx "$SBXD"
  KB="$(cli sandbox exec "$SBXD" -- sh -c 'df -k / | tail -1' 2>/dev/null | awk '{print $2}')"
  if [[ "$KB" =~ ^[0-9]+$ && "$KB" -ge 1800000 ]]; then
    pass "rootfs grew to ~$((KB/1024/1024))G (df: ${KB}K)"
  else
    fail "--disk 2G: rootfs total = ${KB:-?}K, want >= ~1.8G"
  fi
else fail "run $ALPINE --disk 2G failed: $SBXD"; fi

# ---- 08 graceful stop + remove ----------------------------------------------
echo "== 08 stop (graceful) then rm"
if [[ -n "${SBX:-}" && "$SBX" == sbx_* ]]; then
  if cli stop "$SBX" >/dev/null 2>&1 && cli sandbox ls | grep -q "$SBX"; then
    pass "stop halted the service but kept the sandbox"
  else
    fail "stop did not keep the sandbox (or errored)"
  fi
  cli rm "$SBX" >/dev/null 2>&1
  sleep 1
  if ! cli sandbox ls | grep -q "$SBX"; then pass "rm removed the sandbox"; else fail "rm left $SBX behind"; fi
else skip "no sandbox"; fi

# ---- 09 build a Dockerfile + run it -----------------------------------------
echo "== 09 crucible build + run the result"
if command -v docker >/dev/null 2>&1; then
  DIG="$(cli build -t crucible-installed-test "$HERE/testapp" 2>/dev/null)"
  if [[ "$DIG" == sha256:* ]]; then
    track_img "$DIG"
    SBXB="$(cli run "$DIG" -p "$HOST_PORT_B:80" --memory 256 2>/dev/null)"
    if [[ "$SBXB" == sbx_* ]]; then
      track_sbx "$SBXB"
      if hit "http://localhost:$HOST_PORT_B/" "CRUCIBLE-TESTAPP-OK"; then
        pass "built image booted + served distinctive content on :$HOST_PORT_B"
      else fail "built image unreachable on :$HOST_PORT_B"; fi
    else fail "run of built image failed: $SBXB"; fi
    # v0.4.2: an app from the built image (which declares a HEALTHCHECK) and no
    # --health of its own inherits the image's HEALTHCHECK as a seeded exec check.
    if curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
      SEEDAPP="crucible-smoke-seed"
      cli app rm "$SEEDAPP" >/dev/null 2>&1 || true
      if [[ "$(cli app create "$SEEDAPP" --image "$DIG" --memory 256 2>/dev/null)" == "$SEEDAPP" ]]; then
        track_app "$SEEDAPP"
        SH=0
        for _ in {1..30}; do
          [[ "$(curl -s "$BASE_URL/apps/$SEEDAPP" 2>/dev/null)" == *'"health":"healthy"'* ]] && { SH=1; break; }
          sleep 1
        done
        [[ "$SH" -eq 1 ]] && pass "app seeded health from the image's HEALTHCHECK (healthy, no --health set)" \
          || fail "image HEALTHCHECK not seeded to healthy: $(curl -s "$BASE_URL/apps/$SEEDAPP" 2>&1)"
        cli app rm "$SEEDAPP" >/dev/null 2>&1
      else
        skip "seeding: app create from the built image failed (pre-v0.4.2 daemon?)"
      fi
    fi
  else fail "build produced no digest: $DIG"; fi
else
  skip "docker not installed — crucible build needs it (client-side)"
fi

# ---- 10 durable app: create, serve, health, self-heal, delete (v0.4) --------
echo "== 10 durable app (v0.4): create + serve + http health + self-heal + rm"
APP="crucible-smoke-app"
if ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint (pre-v0.4, or apps not enabled)"
else
  cli app rm "$APP" >/dev/null 2>&1 || true   # free the name from a prior run
  # stdout carries JUST the app name; image import/progress goes to stderr, so
  # capture the streams separately and match stdout exactly.
  APP_ERR="$(mktemp)"
  OUT="$(cli app create "$APP" --image "$IMAGE" -p "$HOST_PORT_C:80" \
           --restart always --health "http:80:/" --memory 256 2>"$APP_ERR")"
  if [[ "$OUT" == "$APP" ]]; then
    track_app "$APP"
    rm -f "$APP_ERR"
    # the app's instance boots and serves the published port
    if hit "http://localhost:$HOST_PORT_C/" "html" || hit "http://localhost:$HOST_PORT_C/" "nginx"; then
      pass "app instance booted and served on :$HOST_PORT_C"
    else
      fail "app never served on :$HOST_PORT_C"
    fi
    # the http health probe reaches the guest and passes
    HEALTHY=0
    for _ in {1..25}; do
      [[ "$(curl -s "$BASE_URL/apps/$APP" 2>/dev/null)" == *'"health":"healthy"'* ]] && { HEALTHY=1; break; }
      sleep 1
    done
    [[ "$HEALTHY" -eq 1 ]] && pass "http health check reports healthy" \
      || fail "app never reported healthy: $(curl -s "$BASE_URL/apps/$APP" 2>&1)"
    # ls surfaces it
    if cli app ls 2>/dev/null | grep -q "$APP"; then pass "app ls lists $APP"; else fail "app ls missing $APP"; fi
    # self-heal (the reconcile machinery, without restarting the daemon): kill the
    # instance out from under the app and confirm a NEW one is re-created + serves.
    INST="$(curl -s "$BASE_URL/apps/$APP" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
    if [[ "$INST" == sbx_* ]]; then
      cli sandbox rm "$INST" >/dev/null 2>&1 || true
      HEALED=0; NEW=""
      for _ in {1..60}; do
        NEW="$(curl -s "$BASE_URL/apps/$APP" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
        if [[ "$NEW" == sbx_* && "$NEW" != "$INST" ]] && curl -sf "http://localhost:$HOST_PORT_C/" >/dev/null 2>&1; then
          HEALED=1; break
        fi
        sleep 1
      done
      [[ "$HEALED" -eq 1 ]] && pass "self-heal: reconciler re-created the instance ($INST → $NEW) and it serves again" \
        || fail "app did not self-heal after its instance was killed ($INST → ${NEW:-none})"
    else
      skip "could not read app instance id for the self-heal check"
    fi
    # delete tears the app AND its instance down (port stops answering)
    cli app rm "$APP" >/dev/null 2>&1
    sleep 3
    if curl -sf "http://localhost:$HOST_PORT_C/" >/dev/null 2>&1; then
      fail "deleted app still serving on :$HOST_PORT_C"
    else
      pass "app rm tore down the app and its instance"
    fi
  else
    # The /apps probe above already confirmed apps are enabled, so a create that
    # doesn't print the name is a real failure, not an unsupported daemon.
    fail "app create did not return the name: stdout=${OUT:-<none>} stderr=$(cat "$APP_ERR" 2>/dev/null)"
    rm -f "$APP_ERR"
  fi
fi

# ---- 11 MCP server drives the daemon (stdio JSON-RPC) -----------------------
echo "== 11 MCP server (crucible mcp serve) end-to-end"
if ! command -v python3 >/dev/null 2>&1; then
  skip "python3 not installed — the MCP smoke drives the stdio protocol with it"
else
  MCP_TMP="$(mktemp -d)"
  MCP_APP="crucible-smoke-mcp"
  track_app "$MCP_APP"   # safety net in case the driver dies mid round-trip
  cli app rm "$MCP_APP" >/dev/null 2>&1 || true
  cat >"$MCP_TMP/mcp_driver.py" <<'PY'
import json, os, signal, subprocess, sys

signal.alarm(240)  # never wedge the smoke on a hung daemon

CRUX = os.environ["CRUX"]; ADDR = os.environ["ADDR"]
IMAGE = os.environ.get("MCP_IMAGE", "alpine:latest")
APP = os.environ.get("MCP_APP", "crucible-smoke-mcp")

p = subprocess.Popen([CRUX, "--addr", ADDR, "mcp", "serve"],
                     stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=sys.stderr, text=True, bufsize=1)

def send(o): p.stdin.write(json.dumps(o) + "\n"); p.stdin.flush()
def read(): return json.loads(p.stdout.readline())
def die(msg): print("DRIVER-FAIL:", msg); p.kill(); sys.exit(1)

send({"jsonrpc":"2.0","id":1,"method":"initialize",
      "params":{"protocolVersion":"2025-06-18","capabilities":{},
                "clientInfo":{"name":"installed-smoke","version":"0"}}})
read()
send({"jsonrpc":"2.0","method":"notifications/initialized"})

# tools/list — the catalog must advertise the core sandbox tools
send({"jsonrpc":"2.0","id":2,"method":"tools/list"})
tools = sorted(t["name"] for t in read()["result"]["tools"])
print("tools/list (%d): %s" % (len(tools), tools))
for t in ("run","exec","create_sandbox","delete_sandbox","list_profiles"):
    if t not in tools: die("catalog missing core tool %r" % t)

def call(name, args):
    send({"jsonrpc":"2.0","id":99,"method":"tools/call",
          "params":{"name":name,"arguments":args}})
    return read()["result"]

# run round-trip: MCP boots a VM, runs a command, tears it down
r = call("run", {"image":IMAGE, "command":["sh","-c","echo mcp-run-ok"]})
if r.get("isError"): die("run errored: %s" % r["content"])
out = r["structuredContent"]
if out["exit_code"] != 0 or "mcp-run-ok" not in out["stdout"]:
    die("run output wrong: %r" % out)
print("run: exit=%d stdout=%r" % (out["exit_code"], out["stdout"]))
print("MCP-CORE-OK")

# app tools (v0.4): create stopped (no VM boot) → get → list → delete
app_tools = {"create_app","list_apps","get_app","delete_app"}
if not app_tools.issubset(tools):
    print("MCP-APPS-SKIP (app tools not advertised)")
else:
    # stopped=True → no VM boot; also exercises the v0.4.1 tool args (env,
    # net_full_egress, publish_all) so a schema/plumbing regression is caught.
    r = call("create_app", {"name":APP, "image":IMAGE, "stopped":True,
                            "env":["MCP_ENV=ok"], "net_full_egress":True, "publish_all":True})
    if r.get("isError"): die("create_app errored: %s" % r["content"])
    if not r["structuredContent"]["id"].startswith("app_"):
        die("create_app returned no app id: %r" % r["structuredContent"])
    r = call("get_app", {"name":APP})
    if r.get("isError") or r["structuredContent"]["name"] != APP:
        die("get_app wrong: %r" % r.get("structuredContent"))
    r = call("list_apps", {})
    names = [a["name"] for a in r["structuredContent"]["apps"]]
    if APP not in names: die("list_apps missing %r: %s" % (APP, names))
    r = call("delete_app", {"name":APP})
    if r.get("isError"): die("delete_app errored: %s" % r["content"])
    print("app tools: create/get/list/delete round-trip ok")
    print("MCP-APPS-OK")

# v0.4.3: the operate-an-app-by-name tools must be advertised in the catalog.
if {"app_exec","app_logs"}.issubset(tools):
    print("MCP-APPOPS-OK")
else:
    print("MCP-APPOPS-SKIP (app_exec/app_logs not advertised)")

p.stdin.close()
try: p.wait(timeout=10)
except Exception: p.kill()
PY
  MCP_OUT="$(CRUX="$CRUCIBLE_BIN" ADDR="$ADDR" MCP_IMAGE="$ALPINE" MCP_APP="$MCP_APP" \
             python3 "$MCP_TMP/mcp_driver.py" 2>&1)"; MCP_RC=$?
  echo "$MCP_OUT" | sed 's/^/     /'
  if [[ "$MCP_RC" -eq 0 && "$MCP_OUT" == *"MCP-CORE-OK"* ]]; then
    pass "MCP core: tools/list advertises the catalog + run round-trip through the daemon"
  else
    fail "MCP core checks failed (rc=$MCP_RC)"
  fi
  if [[ "$MCP_OUT" == *"MCP-APPS-OK"* ]]; then
    pass "MCP app tools: create/get/list/delete round-trip"
  elif [[ "$MCP_OUT" == *"MCP-APPS-SKIP"* ]]; then
    skip "MCP app tools not advertised (pre-v0.4 daemon)"
  fi
  if [[ "$MCP_OUT" == *"MCP-APPOPS-OK"* ]]; then
    pass "MCP v0.4.3: app_exec/app_logs advertised in the catalog"
  elif [[ "$MCP_OUT" == *"MCP-APPOPS-SKIP"* ]]; then
    skip "MCP app_exec/app_logs not advertised (pre-v0.4.3 daemon)"
  fi
fi

# ---- 12 v0.4.1: app --env + exec health check -------------------------------
echo "== 12 v0.4.1 app: --env + --health-cmd (exec health)"
if ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint"
else
  APP2="crucible-smoke-app2"
  cli app rm "$APP2" >/dev/null 2>&1 || true
  A2ERR="$(mktemp)"
  OUT="$(cli app create "$APP2" --image "$IMAGE" -p "$HOST_PORT_D:80" \
           -e SMOKE_ENV=ok --health-cmd 'test -f /etc/nginx/nginx.conf' --memory 256 2>"$A2ERR")"
  if [[ "$OUT" == "$APP2" ]]; then
    track_app "$APP2"; rm -f "$A2ERR"
    if hit "http://localhost:$HOST_PORT_D/" "html" || hit "http://localhost:$HOST_PORT_D/" "nginx"; then
      pass "app with --env booted and served on :$HOST_PORT_D"
    else
      fail "v0.4.1 app never served on :$HOST_PORT_D"
    fi
    H=0
    for _ in {1..25}; do
      [[ "$(curl -s "$BASE_URL/apps/$APP2" 2>/dev/null)" == *'"health":"healthy"'* ]] && { H=1; break; }
      sleep 1
    done
    [[ "$H" -eq 1 ]] && pass "exec health check (--health-cmd) reports healthy" \
      || fail "exec health never healthy: $(curl -s "$BASE_URL/apps/$APP2" 2>&1)"
    cli app rm "$APP2" >/dev/null 2>&1
  else
    skip "v0.4.1 app flags unsupported (pre-v0.4.1 daemon): $(head -1 "$A2ERR" 2>/dev/null)"; rm -f "$A2ERR"
  fi
fi

# ---- 13 v0.4.1 egress modes: full-egress (+ SSRF tripwire) and CIDR ---------
# reach <sbx> <host> <port> — 0 if the guest can TCP-connect (busybox nc).
reach() { cli sandbox exec "$1" -- sh -c "nc -w 4 $2 $3 </dev/null" >/dev/null 2>&1; }

echo "== 13a full-egress reaches a public host but refuses cloud metadata"
SBXE="$(cli sandbox create --image "$ALPINE" --memory 256 --net-full-egress 2>/dev/null)"
if [[ "$SBXE" == sbx_* ]]; then
  track_sbx "$SBXE"
  reach "$SBXE" 1.1.1.1 443 && pass "full-egress reached a public host (1.1.1.1:443)" \
    || fail "full-egress could not reach a public host (is this host online?)"
  # The tripwire: metadata + RFC1918 MUST stay unreachable even under full-egress.
  reach "$SBXE" 169.254.169.254 80 && fail "SSRF: full-egress reached cloud metadata — guard regressed!" \
    || pass "full-egress refused cloud metadata 169.254.169.254 (SSRF guard holds)"
  reach "$SBXE" 10.255.255.1 80 && fail "SSRF: full-egress reached RFC1918 — guard regressed!" \
    || pass "full-egress refused RFC1918 10.255.255.1"
  cli sandbox rm "$SBXE" >/dev/null 2>&1
else
  skip "full-egress unsupported (pre-v0.4.1 daemon) or daemon has no --network-egress-iface"
fi

echo "== 13b CIDR: in-range public reachable, out-of-range not"
SBXC="$(cli sandbox create --image "$ALPINE" --memory 256 --net-allow-cidr 1.1.1.0/24 2>/dev/null)"
if [[ "$SBXC" == sbx_* ]]; then
  track_sbx "$SBXC"
  reach "$SBXC" 1.1.1.1 443 && pass "CIDR 1.1.1.0/24 reached in-range 1.1.1.1" \
    || fail "CIDR did not reach an in-range public host"
  reach "$SBXC" 8.8.8.8 443 && fail "out-of-range 8.8.8.8 reachable (CIDR leaked)" \
    || pass "out-of-range 8.8.8.8 not reachable"
  cli sandbox rm "$SBXC" >/dev/null 2>&1
else
  skip "--net-allow-cidr unsupported (pre-v0.4.1 daemon)"
fi

# ---- 14 v0.4.1: -P publishes the image's EXPOSEd port -----------------------
echo "== 14 -P publishes the image's EXPOSEd port (guest 80 → host 80)"
# -P maps the image's EXPOSEd :80 to host :80, so anything ALREADY bound to :80
# would clash. Detect a listener by connectivity, not a 2xx — the ingress proxy
# (now on :80 by default) answers an unmatched Host with 404, which `curl -sf`
# would miss, letting -P run into a guaranteed bind clash.
if [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://localhost:80/ 2>/dev/null)" != "000" ]]; then
  skip "-P check: something already listens on :80 (e.g. the ingress proxy) — -P to host :80 would clash"
elif ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint"
else
  PAPP="crucible-smoke-puball"
  cli app rm "$PAPP" >/dev/null 2>&1 || true
  POUT="$(cli app create "$PAPP" --image "$IMAGE" -P --memory 256 2>/dev/null)"
  if [[ "$POUT" == "$PAPP" ]]; then
    track_app "$PAPP"
    if hit "http://localhost:80/" "html" || hit "http://localhost:80/" "nginx"; then
      pass "-P published the image's EXPOSEd port; served on :80 with no explicit -p"
    else
      fail "-P did not publish the EXPOSEd port (:80 unreachable)"
    fi
    cli app rm "$PAPP" >/dev/null 2>&1
  else
    skip "-P/--publish-all unsupported (pre-v0.4.1 daemon)"
  fi
fi

# ---- 15 v0.4.2: app update replaces the spec and redeploys ------------------
echo "== 15 app update: replace the spec and redeploy the instance (v0.4.2)"
if ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint"
else
  UAPP="crucible-smoke-upd"
  cli app rm "$UAPP" >/dev/null 2>&1 || true
  UERR="$(mktemp)"
  OUT="$(cli app create "$UAPP" --image "$IMAGE" -p "$HOST_PORT_E:80" --restart always --memory 256 2>"$UERR")"
  if [[ "$OUT" == "$UAPP" ]]; then
    track_app "$UAPP"; rm -f "$UERR"
    if hit "http://localhost:$HOST_PORT_E/" "html" || hit "http://localhost:$HOST_PORT_E/" "nginx"; then
      INST1="$(curl -s "$BASE_URL/apps/$UAPP" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
      # Change the spec (memory + a new env) — a generation bump forces a redeploy.
      UPD="$(cli app update "$UAPP" --image "$IMAGE" -p "$HOST_PORT_E:80" --restart always --memory 320 -e UPDATED=yes 2>/dev/null)"
      if [[ "$UPD" == "$UAPP" ]]; then
        REDEPLOYED=0; INST2=""
        for _ in $(seq 1 60); do
          INST2="$(curl -s "$BASE_URL/apps/$UAPP" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
          if [[ "$INST2" == sbx_* && "$INST2" != "$INST1" ]] && curl -sf "http://localhost:$HOST_PORT_E/" >/dev/null 2>&1; then
            REDEPLOYED=1; break
          fi
          sleep 1
        done
        [[ "$REDEPLOYED" -eq 1 ]] && pass "app update redeployed to a new instance ($INST1 → $INST2) and serves" \
          || fail "app update did not redeploy ($INST1 → ${INST2:-none})"
        GEN="$(curl -s "$BASE_URL/apps/$UAPP" 2>/dev/null | grep -o '"generation":[0-9]*' | grep -o '[0-9]*' | head -1)"
        [[ "$GEN" == "2" ]] && pass "generation bumped to 2 after update" || fail "generation = ${GEN:-?}, want 2"
      else
        skip "app update unsupported (pre-v0.4.2 daemon)"
      fi
    else
      fail "update app never served on :$HOST_PORT_E"
    fi
    cli app rm "$UAPP" >/dev/null 2>&1
  else
    skip "app create for the update test failed: $(head -1 "$UERR" 2>/dev/null)"; rm -f "$UERR"
  fi
fi

# ---- 16 v0.4.2: reach an app by name through the ingress proxy (opt-in) ------
echo "== 16 ingress proxy: reach an app by name (set PROXY_ADDR + PROXY_DOMAIN)"
if [[ -z "${PROXY_ADDR:-}" ]]; then
  skip "proxy test off — set PROXY_ADDR=host:port (where --proxy-listen binds) and PROXY_DOMAIN (--proxy-domain); the proxy is off by default"
elif ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint"
else
  PDOM="${PROXY_DOMAIN:-apps.local}"
  PXAPP="crucible-smoke-proxy"
  cli app rm "$PXAPP" >/dev/null 2>&1 || true
  if [[ "$(cli app create "$PXAPP" --image "$IMAGE" --port 80 --restart always --memory 256 2>/dev/null)" == "$PXAPP" ]]; then
    track_app "$PXAPP"
    ROUTED=0
    for _ in $(seq 1 40); do
      body="$(curl -s --max-time 3 -H "Host: $PXAPP.$PDOM" "http://$PROXY_ADDR/" 2>/dev/null || true)"
      [[ "$body" == *"html"* || "$body" == *"nginx"* ]] && { ROUTED=1; break; }
      sleep 0.5
    done
    [[ "$ROUTED" -eq 1 ]] && pass "proxy routed $PXAPP.$PDOM → the app's current instance" \
      || fail "proxy did not route $PXAPP.$PDOM via $PROXY_ADDR (is --proxy-listen/--proxy-domain set?)"
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 4 -H "Host: nope.$PDOM" "http://$PROXY_ADDR/" 2>/dev/null)"
    [[ "$code" == "404" ]] && pass "proxy: unknown host → 404" || fail "proxy unknown host → $code, want 404"
    # v0.4.3: a rolling `app update` flips to a new instance while the app stays
    # reachable BY NAME through the proxy (zero-downtime; the drop-free assertion
    # is scripts/smoke_zerodowntime.sh — here we confirm it flips + stays routed).
    PXINST1="$(curl -s "$BASE_URL/apps/$PXAPP" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
    if [[ "$(cli app update "$PXAPP" --image "$IMAGE" --port 80 --restart always --memory 320 2>/dev/null)" == "$PXAPP" ]]; then
      ROLLED=0; PXINST2=""
      for _ in $(seq 1 90); do
        PXINST2="$(curl -s "$BASE_URL/apps/$PXAPP" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
        body="$(curl -s --max-time 3 -H "Host: $PXAPP.$PDOM" "http://$PROXY_ADDR/" 2>/dev/null || true)"
        if [[ "$PXINST2" == sbx_* && "$PXINST2" != "$PXINST1" ]] && [[ "$body" == *"html"* || "$body" == *"nginx"* ]]; then
          ROLLED=1; break
        fi
        sleep 1
      done
      [[ "$ROLLED" -eq 1 ]] && pass "rolling update flipped ($PXINST1 → $PXINST2), still reachable by name" \
        || fail "rolling update did not flip + stay reachable by name ($PXINST1 → ${PXINST2:-none})"
    else
      skip "app update on the proxy app failed (pre-v0.4.3 daemon?)"
    fi
    cli app rm "$PXAPP" >/dev/null 2>&1
  else
    skip "proxy: app create with --port failed (pre-v0.4.2 daemon?)"
  fi
fi

# ---- 17 v0.4.3: operate an app BY NAME — exec + logs (redeploy-safe) ---------
echo "== 17 operate an app by name: app exec + app logs (v0.4.3)"
if ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint"
else
  OAPP="crucible-smoke-ops"
  cli app rm "$OAPP" >/dev/null 2>&1 || true
  # No published port: exec/logs by name go over vsock, not the network, so this
  # exercises the name→current-instance resolution without touching a host port.
  if [[ "$(cli app create "$OAPP" --image "$IMAGE" --restart always --memory 256 2>/dev/null)" == "$OAPP" ]]; then
    track_app "$OAPP"
    OINST=""
    for _ in {1..30}; do
      OINST="$(curl -s "$BASE_URL/apps/$OAPP" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
      [[ "$OINST" == sbx_* ]] && break
      sleep 1
    done
    if [[ "$OINST" != sbx_* ]]; then
      skip "app never got an instance for the by-name ops test"
    else
      EX="$(cli app exec "$OAPP" -- /bin/sh -c 'echo OPS-EXEC-OK' 2>/dev/null)"
      if [[ "$EX" == *OPS-EXEC-OK* ]]; then
        pass "app exec <name> resolved to the current instance and ran"
        if cli app logs "$OAPP" 2>/dev/null | grep -q 'OPS-EXEC-OK'; then
          pass "app logs <name> shows the exec activity"
        else
          fail "app logs <name> did not show the exec activity"
        fi
      else
        skip "app exec by name unsupported (pre-v0.4.3 daemon?): ${EX:-empty}"
      fi
    fi
    cli app rm "$OAPP" >/dev/null 2>&1
  else
    skip "app create for the by-name ops test failed"
  fi
fi

# ---- 18 v0.5.0: scale-to-zero — app sleep + wake in place -------------------
echo "== 18 scale-to-zero (v0.5.0): app sleep frees RAM, wake restores in place"
# app-status helpers (JSON from GET /apps/<name>): phase is a plain string field;
# containment matching mirrors the health checks above so no python3 is needed.
app_phase() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o '"phase":"[a-z]*"' | head -1 | grep -o '[a-z]*"$' | tr -d '"'; }
jnum() { curl -s "$BASE_URL/apps/$1" 2>/dev/null | grep -o "\"$2\":[0-9]*" | head -1 | grep -o '[0-9]*'; }
if ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint"
else
  SAPP="crucible-smoke-sleep"
  cli app rm "$SAPP" >/dev/null 2>&1 || true
  SERR="$(mktemp)"
  OUT="$(cli app create "$SAPP" --image "$IMAGE" -p "$HOST_PORT_F:80" \
           --restart always --health "http:80:/" --memory 256 2>"$SERR")"
  if [[ "$OUT" == "$SAPP" ]]; then
    track_app "$SAPP"; rm -f "$SERR"
    if hit "http://localhost:$HOST_PORT_F/" "html" || hit "http://localhost:$HOST_PORT_F/" "nginx"; then
      pass "sleep-test app booted and served on :$HOST_PORT_F"
    else
      fail "sleep-test app never served on :$HOST_PORT_F"
    fi
    # `app sleep` snapshots the instance and stops the VMM. If the subcommand is
    # unknown (pre-v0.5.0 daemon) this errors and we skip the rest.
    if cli app sleep "$SAPP" >/dev/null 2>&1; then
      ASLEEP=0
      for _ in {1..30}; do [[ "$(app_phase "$SAPP")" == "asleep" ]] && { ASLEEP=1; break; }; sleep 1; done
      [[ "$ASLEEP" -eq 1 ]] && pass "app sleep → phase=asleep (VMM stopped, RAM freed)" \
        || fail "app never reached phase=asleep: $(curl -s "$BASE_URL/apps/$SAPP" 2>&1)"
      # while asleep the published port stops answering
      curl -sf "http://localhost:$HOST_PORT_F/" >/dev/null 2>&1 \
        && echo "   (note: published port still answered while asleep)" \
        || pass "published port stops serving while asleep"
      # `app wake` restores in place: phase=running, serves again, reports latency
      cli app wake "$SAPP" >/dev/null 2>&1
      WOKE=0
      for _ in {1..40}; do
        [[ "$(app_phase "$SAPP")" == "running" ]] && curl -sf "http://localhost:$HOST_PORT_F/" >/dev/null 2>&1 && { WOKE=1; break; }
        sleep 1
      done
      if [[ "$WOKE" -eq 1 ]]; then
        pass "app wake restored in place (phase=running, serving again)"
        MS="$(jnum "$SAPP" last_wake_latency_ms)"
        [[ "${MS:-0}" -gt 0 ]] 2>/dev/null && pass "status reports last_wake_latency_ms=$MS" \
          || echo "   (note: last_wake_latency_ms=${MS:-0})"
      else
        APPJSON="$(curl -s "$BASE_URL/apps/$SAPP" 2>/dev/null)"
        # A stale rootfs whose baked crucible-agent predates wake support reports
        # this exact error. That's an old *install*, not a wake regression — self-
        # skip (like the pre-v0.5.0 branch above), pointing at the real fix.
        if [[ "$APPJSON" == *"does not support wake"* ]]; then
          skip "installed rootfs predates wake support — rebuild the guest agent + rootfs (make agent && make build, then reinstall) so v0.5.0 wake is baked in"
        else
          fail "app wake did not restore service: $APPJSON"
        fi
      fi
    else
      skip "app sleep/wake unsupported (pre-v0.5.0 daemon)"
    fi
    cli app rm "$SAPP" >/dev/null 2>&1
  else
    skip "sleep-test app create failed: $(head -1 "$SERR" 2>/dev/null)"; rm -f "$SERR"
  fi
fi

# ---- 19 v0.5.1: app→app networking — grant / default-deny / peer isolation --
echo "== 19 app→app networking (v0.5.1): <app>.internal grant + default-deny"
# Requires the daemon started with --internal-networking (EXPERIMENTAL, OFF by
# default). We can't read the daemon's flags from a client, so we PROBE: create a
# caller granted --can-call the backend and try the internal call. If it doesn't
# resolve/serve, internal networking isn't enabled here → SKIP (not a failure).
# Once the granted path works the feature IS on, so the deny + isolation checks
# become HARD assertions (a security regression must fail, like the SSRF tripwire).
if ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint"
else
  BACK="crucible-smoke-backend"; SECRET="crucible-smoke-secret"; CALL="crucible-smoke-caller"
  for a in "$BACK" "$SECRET" "$CALL"; do cli app rm "$a" >/dev/null 2>&1 || true; done
  A2A_ERR="$(mktemp)"
  c_ok=1
  cli app create "$BACK"   --image "$IMAGE" --port 80 --restart always --memory 256 >/dev/null 2>>"$A2A_ERR" || c_ok=0
  cli app create "$SECRET" --image "$IMAGE" --port 80 --restart always --memory 256 >/dev/null 2>>"$A2A_ERR" || c_ok=0
  cli app create "$CALL"   --image "$IMAGE" --port 80 --restart always --memory 256 --can-call "$BACK" >/dev/null 2>>"$A2A_ERR" || c_ok=0
  if [[ "$c_ok" -ne 1 ]]; then
    skip "app→app create failed (pre-v0.5.1 daemon? --can-call unsupported): $(head -1 "$A2A_ERR" 2>/dev/null)"
    for a in "$BACK" "$SECRET" "$CALL"; do cli app rm "$a" >/dev/null 2>&1 || true; done
    rm -f "$A2A_ERR"
  else
    track_app "$BACK"; track_app "$SECRET"; track_app "$CALL"; rm -f "$A2A_ERR"
    # wait for the caller to have a live instance to exec into
    CINST=""
    for _ in {1..40}; do
      CINST="$(curl -s "$BASE_URL/apps/$CALL" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
      [[ "$CINST" == sbx_* ]] && break; sleep 1
    done
    # PROBE the granted internal call (default internal VIP port is 80).
    GOT=""
    for _ in {1..30}; do
      GOT="$(cli app exec "$CALL" -- wget -T 5 -q -O - "http://$BACK.internal/" 2>/dev/null)"
      [[ "$GOT" == *nginx* || "$GOT" == *"<html"* ]] && break; sleep 1
    done
    if [[ "$GOT" != *nginx* && "$GOT" != *"<html"* ]]; then
      skip "internal networking not enabled (daemon needs --internal-networking) — granted call to $BACK.internal did not resolve"
    else
      pass "GRANTED: caller reached $BACK.internal (served $(echo "$GOT" | wc -c) bytes)"
      # HARD: an un-granted peer must be refused (default-deny; no body served).
      DENIED="$(cli app exec "$CALL" -- wget -T 5 -q -O - "http://$SECRET.internal/" 2>/dev/null)"
      if [[ -z "$DENIED" || ( "$DENIED" != *nginx* && "$DENIED" != *"<html"* ) ]]; then
        pass "DEFAULT-DENY: un-granted caller→$SECRET.internal refused (no body served)"
      else
        fail "SECURITY: un-granted caller reached $SECRET.internal! (got: '${DENIED:0:60}')"
      fi
      # HARD: peer isolation — caller must NOT reach backend's guest IP directly.
      BINST="$(curl -s "$BASE_URL/apps/$BACK" 2>/dev/null | grep -o '"instance_id":"sbx_[a-z0-9]*"' | grep -o 'sbx_[a-z0-9]*' | head -1)"
      BIP="$(curl -s "$BASE_URL/sandboxes/$BINST" 2>/dev/null | grep -o '"guest_ip":"[0-9.]*"' | grep -o '[0-9.]*' | head -1)"
      if [[ -n "$BIP" ]]; then
        if cli app exec "$CALL" -- sh -c "nc -w 3 $BIP 80 </dev/null" >/dev/null 2>&1; then
          fail "SECURITY: caller reached backend guest IP $BIP directly — lateral isolation broken!"
        else
          pass "PEER ISOLATION: caller cannot reach backend guest IP $BIP directly (VIP is the only path)"
        fi
      else
        skip "could not read backend guest IP for the isolation check"
      fi
    fi
    for a in "$BACK" "$SECRET" "$CALL"; do cli app rm "$a" >/dev/null 2>&1 || true; done
  fi
fi

# ---- 20 v0.5.2: horizontal scale-out — a --min-scale N app runs N replicas ---
echo "== 20 horizontal scale-out (v0.5.2): --min-scale 2 converges to 2 replicas"
# Acceptance-friendly proof of the scale-out machinery: a fixed floor of 2 warm
# replicas (no slow autoscale-under-load dance — that's scripts/smoke_app_scaleout.sh).
# Observable purely from the app status (replicas / ready_replicas / instances),
# so no published port is consumed.
if ! curl -sf "$BASE_URL/apps" >/dev/null 2>&1; then
  skip "daemon has no /apps endpoint"
else
  SOAPP="crucible-smoke-scaleout"
  cli app rm "$SOAPP" >/dev/null 2>&1 || true
  SOERR="$(mktemp)"
  OUT="$(cli app create "$SOAPP" --image "$IMAGE" --port 80 --restart always --memory 256 \
           --min-scale 2 --max-scale 4 2>"$SOERR")"
  if [[ "$OUT" == "$SOAPP" ]]; then
    track_app "$SOAPP"; rm -f "$SOERR"
    # the fleet converges to 2 ready replicas (extras warm-forked from a golden snap)
    READY=0
    for _ in {1..60}; do
      R="$(jnum "$SOAPP" ready_replicas)"
      [[ "${R:-0}" -ge 2 ]] 2>/dev/null && { READY=1; break; }
      sleep 2
    done
    if [[ "$READY" -eq 1 ]]; then
      pass "converged to $(jnum "$SOAPP" ready_replicas) ready replicas ($(jnum "$SOAPP" replicas) desired)"
    else
      fail "did not reach 2 ready replicas: ready=$(jnum "$SOAPP" ready_replicas) desired=$(jnum "$SOAPP" replicas)"
    fi
    # the status lists a distinct instance per replica (endpoint set)
    if command -v python3 >/dev/null 2>&1; then
      NINST="$(curl -s "$BASE_URL/apps/$SOAPP" 2>/dev/null | python3 -c 'import json,sys; s=json.load(sys.stdin).get("status",{}); ids={i.get("instance_id") for i in s.get("instances",[])}; print(len([x for x in ids if x]))' 2>/dev/null)"
      [[ "${NINST:-0}" -ge 2 ]] 2>/dev/null && pass "endpoint set lists $NINST distinct instances" \
        || fail "endpoint set has ${NINST:-0} instances, want >= 2"
    fi
    cli app rm "$SOAPP" >/dev/null 2>&1
  else
    skip "scale-out unsupported (pre-v0.5.2 daemon? --min-scale/--max-scale): $(head -1 "$SOERR" 2>/dev/null)"; rm -f "$SOERR"
  fi
fi

# ---- summary ----------------------------------------------------------------
echo "==============================================================="
echo " installed-release acceptance: $PASS passed, $FAIL failed, $SKIP skipped"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
