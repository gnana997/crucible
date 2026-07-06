#!/usr/bin/env bash
#
# Integration smoke for the MCP layer.
#
# Drives `crucible mcp serve` (stdio) against a REAL daemon and asserts:
#   01  initialize handshake + tools/list advertises the full catalog
#   02  list_profiles returns the daemon's profiles
#   03  run: create → exec → delete round-trip returns the command's output
#   04  create_sandbox → exec → delete against a persistent sandbox
#   05  a guardrail bites: --net-allow-max rejects an out-of-ceiling host
#   06  (optional) daemon API-key auth: no token fails, --token succeeds
#
# The MCP server is a thin client of the daemon, so this exercises the whole
# path: stdio JSON-RPC → internal/mcpserver → internal/client → daemon → VM.
#
# Requires (same as smoke_e2e.sh):
#   - Linux host with KVM + root (jailer needs CAP_SYS_ADMIN)
#   - Guest rootfs with crucible-agent (a shell + echo is enough here)
#   - crucible binaries built (make build && make agent && make rootfs)
#   - python3 on the host (drives the MCP stdio protocol)
#
# KVM access (two jailer host requirements this script handles for you):
#   1. drop-gid — the jailer runs firecracker as an unprivileged gid, which must
#      be able to open /dev/kvm (root:kvm, 660), i.e. the kvm group. Auto-detected
#      and passed as --jail-gid (override with JAIL_GID=...).
#   2. chroot fs — the jailer mknod's /dev/kvm inside the chroot, so CHROOT_BASE
#      must be on a filesystem that allows device nodes. /tmp is usually tmpfs+
#      nodev (node inert → EACCES), so the chroot defaults to /srv/jailer, not the
#      /tmp smoke dir. Override with CHROOT_BASE=... on a dev-allowing fs.
#
# Usage:
#   sudo CRUCIBLE_BIN=./crucible \
#        FIRECRACKER_BIN=/path/to/firecracker \
#        JAILER_BIN=/path/to/jailer \
#        KERNEL=/path/to/vmlinux \
#        ROOTFS=./assets/rootfs-with-agent.ext4 \
#        scripts/smoke_mcp.sh
#
set -euo pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:?set FIRECRACKER_BIN}"
JAILER_BIN="${JAILER_BIN:?set JAILER_BIN}"
KERNEL="${KERNEL:?set KERNEL}"
ROOTFS="${ROOTFS:?set ROOTFS}"
PROFILE="${PROFILE:-}"                 # optional --default-profile for the server

PORT="${PORT:-7899}"
ADDR="http://127.0.0.1:${PORT}"
BASE_URL="$ADDR"
LISTEN="127.0.0.1:${PORT}"

# The jailer drops firecracker to an unprivileged gid, and firecracker reaches
# /dev/kvm (root:kvm, mode 660) through its *group*. So the drop-gid must be the
# kvm group, or the jailed VM gets EACCES creating the KVM object. Auto-detect it
# (override with JAIL_GID=...); fall back to the daemon default if there's no kvm
# group (e.g. a host where /dev/kvm is world-accessible).
JAIL_GID="${JAIL_GID:-$(getent group kvm 2>/dev/null | cut -d: -f3 || true)}"

SMOKE_ROOT="$(mktemp -d /tmp/crucible-mcp-smoke-XXXXXX)"
WORK_BASE="$SMOKE_ROOT/work"
TOKEN_FILE="$SMOKE_ROOT/tokens.json"
DAEMON_LOG="$SMOKE_ROOT/daemon.log"
# The jailer mknod's /dev/kvm inside the chroot, so the chroot MUST live on a
# filesystem that allows device nodes. /tmp is commonly tmpfs+nodev, which makes
# that node inert (EACCES opening KVM) — so default the chroot base off /tmp,
# same as smoke_e2e.sh. Override with CHROOT_BASE=... on a dev-allowing fs.
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
mkdir -p "$WORK_BASE" "$CHROOT_BASE"

echo " smoke root       : $SMOKE_ROOT"
echo " daemon log       : $DAEMON_LOG"
echo " crucible binary  : $CRUCIBLE_BIN"
echo " address          : $ADDR"
echo " chroot base      : $CHROOT_BASE"
echo " jailer drop-gid  : ${JAIL_GID:-<daemon default>}"

DAEMON_PID=""

start_daemon() {
  echo "== starting daemon ($*)"
  local gidflag=()
  [[ -n "$JAIL_GID" ]] && gidflag=(--jail-gid "$JAIL_GID")
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" \
    --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" \
    --kernel "$KERNEL" \
    --rootfs "$ROOTFS" \
    --work-base "$WORK_BASE" \
    --log-format json --log-level info \
    "${gidflag[@]}" \
    "$@" \
    >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in {1..100}; do
    if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
      echo "   healthy (pid $DAEMON_PID)"
      return 0
    fi
    if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
      echo "error: daemon exited before becoming healthy" >&2
      tail -40 "$DAEMON_LOG" >&2
      exit 3
    fi
    sleep 0.2
  done
  echo "error: daemon never became healthy" >&2
  tail -40 "$DAEMON_LOG" >&2
  exit 3
}

stop_daemon() {
  if [[ -n "${DAEMON_PID:-}" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill -TERM "$DAEMON_PID" 2>/dev/null || true
    for _ in {1..50}; do kill -0 "$DAEMON_PID" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    DAEMON_PID=""
  fi
}

trap 'stop_daemon' EXIT

# driver <token> <extra-serve-args...>  — runs the python MCP client against
# `crucible <addr> [--token T] mcp serve <extra>` and asserts core behavior.
driver() {
  local token="$1"; shift
  CRUX="$CRUCIBLE_BIN" ADDR="$ADDR" TOKEN="$token" DEFAULT_PROFILE="$PROFILE" \
    python3 "$SMOKE_ROOT/driver.py" "$@"
}

cat >"$SMOKE_ROOT/driver.py" <<'PY'
import json, os, subprocess, sys

CRUX = os.environ["CRUX"]
ADDR = os.environ["ADDR"]
TOKEN = os.environ.get("TOKEN", "")
DEFAULT_PROFILE = os.environ.get("DEFAULT_PROFILE", "")
EXTRA = sys.argv[1:]  # extra `mcp serve` args

argv = [CRUX, "--addr", ADDR]
if TOKEN:
    argv += ["--token", TOKEN]
argv += ["mcp", "serve"]
if DEFAULT_PROFILE:
    argv += ["--default-profile", DEFAULT_PROFILE]
argv += EXTRA

p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=sys.stderr, text=True, bufsize=1)

def send(o): p.stdin.write(json.dumps(o) + "\n"); p.stdin.flush()
def read(): return json.loads(p.stdout.readline())

def fail(msg):
    print("FAIL:", msg); p.kill(); sys.exit(1)

send({"jsonrpc":"2.0","id":1,"method":"initialize",
      "params":{"protocolVersion":"2025-06-18","capabilities":{},
                "clientInfo":{"name":"smoke","version":"0"}}})
read()
send({"jsonrpc":"2.0","method":"notifications/initialized"})

# 01 tools/list
send({"jsonrpc":"2.0","id":2,"method":"tools/list"})
tools = sorted(t["name"] for t in read()["result"]["tools"])
print("tools/list:", tools)
if "run" not in tools or "fork" not in tools:
    fail("catalog missing core tools: %s" % tools)

def call(name, args):
    send({"jsonrpc":"2.0","id":99,"method":"tools/call",
          "params":{"name":name,"arguments":args}})
    return read()["result"]

# 02 list_profiles
r = call("list_profiles", {})
if r.get("isError"): fail("list_profiles errored: %s" % r["content"])
print("list_profiles:", r["structuredContent"]["profiles"])

# 03 run round-trip
r = call("run", {"command":["sh","-c","echo crucible-mcp-ok"]})
if r.get("isError"): fail("run errored: %s" % r["content"])
out = r["structuredContent"]
if out["exit_code"] != 0 or "crucible-mcp-ok" not in out["stdout"]:
    fail("run output wrong: %r" % out)
print("run:", "exit=%d stdout=%r" % (out["exit_code"], out["stdout"]))

# 04 create → exec → delete
r = call("create_sandbox", {})
if r.get("isError"): fail("create_sandbox errored: %s" % r["content"])
sid = r["structuredContent"]["id"]
r = call("exec", {"sandbox_id": sid, "command":["sh","-c","echo exec-ok"]})
if r.get("isError"): fail("exec errored: %s" % r["content"])
if "exec-ok" not in r["structuredContent"]["stdout"]:
    fail("exec output wrong: %r" % r["structuredContent"])
r = call("delete_sandbox", {"sandbox_id": sid})
if r.get("isError"): fail("delete_sandbox errored: %s" % r["content"])
print("create/exec/delete:", sid, "ok")

# 05 guardrail: --net-allow-max must reject an out-of-ceiling host, when set
if "--net-allow-max" in EXTRA:
    r = call("run", {"command":["true"], "net_allow":["evil.example"]})
    if not r.get("isError"):
        fail("--net-allow-max did not reject evil.example")
    print("guardrail net-allow-max: rejected as expected")

p.stdin.close()
try: p.wait(timeout=10)
except Exception: p.kill()
print("driver OK")
PY

# ---- run 1: no auth, with a guardrail flag --------------------------
start_daemon
driver "" --net-allow-max pypi.org
stop_daemon

# ---- run 2 (optional): daemon API-key auth --------------------------
# A token in the store turns auth on even on loopback: no token must fail,
# the right token must work.
echo "== auth: minting a token"
KEY="$("$CRUCIBLE_BIN" daemon token add --token-file "$TOKEN_FILE" --name smoke | grep -o 'crucible_[A-Za-z0-9_-]*')"
[[ -n "$KEY" ]] || { echo "error: could not mint token" >&2; exit 4; }
start_daemon --token-file "$TOKEN_FILE"

echo "== auth: expecting failure WITHOUT a token"
if driver "" >/dev/null 2>&1; then
  echo "FAIL: MCP run succeeded against an authenticated daemon with no token" >&2
  exit 5
fi
echo "   correctly refused"

echo "== auth: expecting success WITH the token"
driver "$KEY"
stop_daemon

echo "ALL MCP SMOKE CHECKS PASSED"
