#!/usr/bin/env bash
#
# Release-acceptance smoke against the ALREADY-RUNNING installed daemon.
#
# Unlike the other smokes (which spin up their own daemon), this drives the
# `crucible` you'd get from a real install — the binary on your PATH talking to
# the systemd-managed daemon at 127.0.0.1:7878 — through the full user journey.
# It answers one question: "will someone who installs the release hit a wall?"
#
# Safe by construction:
#   - runs UNPRIVILEGED (the CLI is just a client; the root daemon does the work)
#   - creates its own sandboxes/snapshots/image and deletes ONLY those on exit
#   - never lists-and-deletes, never stops or restarts the daemon
#
# Prereqs: the daemon is running with image + durable logs enabled
#   (--image-dir and --log-dir in CRUCIBLE_FLAGS) and a default egress iface;
#   internet to pull the public images; curl; docker (only for the build step).
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
CREATED_SBX=(); CREATED_SNAP=(); CREATED_IMG=()
track_sbx()  { CREATED_SBX+=("$1"); }
track_snap() { CREATED_SNAP+=("$1"); }
track_img()  { CREATED_IMG+=("$1"); }

cleanup() {
  echo "== cleanup (only what this smoke created)"
  for id in "${CREATED_SBX[@]:-}"; do [[ -n "$id" ]] && cli sandbox rm "$id" >/dev/null 2>&1 || true; done
  for id in "${CREATED_SNAP[@]:-}"; do [[ -n "$id" ]] && cli snapshot rm "$id" >/dev/null 2>&1 || true; done
  for id in "${CREATED_IMG[@]:-}"; do [[ -n "$id" ]] && cli image rm "$id" >/dev/null 2>&1 || true; done
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
echo "== 00 daemon reachable and on v0.3.x"
if ! curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
  echo "error: no daemon at $BASE_URL — start it: sudo systemctl start crucible" >&2
  exit 3
fi
VER="$(cli version 2>&1)"
if [[ "$VER" == *"v0.3."* ]]; then pass "daemon healthy, CLI is $VER"
else fail "unexpected version: $VER (want v0.3.x)"; fi

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

# ---- summary ----------------------------------------------------------------
echo "==============================================================="
echo " installed-release acceptance: $PASS passed, $FAIL failed, $SKIP skipped"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
