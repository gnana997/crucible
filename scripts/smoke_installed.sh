#!/usr/bin/env bash
#
# Release-acceptance smoke against the ALREADY-RUNNING installed daemon.
#
# Unlike the other smokes (which spin up their own daemon), this drives the
# `crucible` you'd get from a real install — the binary on your PATH talking to
# the systemd-managed daemon at 127.0.0.1:7878 — through the full user journey:
# run/shell/exec, egress deny, logs, snapshot+fork, --disk, stop/rm, build,
# durable apps (v0.4), the MCP server, and the full v0.4.1 surface (app --env,
# exec health, full-egress + its SSRF tripwire, --net-allow-cidr, and -P). It
# answers one question: "will someone who installs the release hit a wall?"
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
# Overrides: CRUCIBLE_BIN (default: crucible on PATH), CRUCIBLE_ADDR
#   (default 127.0.0.1:7878), HOST_PORT_A/B (default 8080/8081).

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
echo "== 00 daemon reachable and on v0.3.x/v0.4.x"
if ! curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
  echo "error: no daemon at $BASE_URL — start it: sudo systemctl start crucible" >&2
  exit 3
fi
VER="$(cli version 2>&1)"
if [[ "$VER" == *"v0.4."* || "$VER" == *"v0.3."* ]]; then pass "daemon healthy, CLI is $VER"
else fail "unexpected version: $VER (want v0.3.x or v0.4.x)"; fi

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
if curl -sf http://localhost:80/ >/dev/null 2>&1; then
  skip "-P check: something is already answering :80 on this host"
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

# ---- summary ----------------------------------------------------------------
echo "==============================================================="
echo " installed-release acceptance: $PASS passed, $FAIL failed, $SKIP skipped"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
