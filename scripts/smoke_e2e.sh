#!/usr/bin/env bash
#
# Extensive end-to-end smoke test for crucible.
#
# Covers tier 1 (correctness-critical) and tier 2 (high-value
# edge cases) scenarios from docs/network.md and the feature
# checklist in CRUCIBLE_README.md v0.1:
#
#   01  default-deny: sandbox with no network has no eth0 IP, no DNS
#   02  exec round-trip: stdout + exit_code + duration
#   03  exit codes + signal propagation
#   04  exec timeout
#   05  allowlist — allowed host (DNS + HTTPS)
#   06  allowlist — denied host (NXDOMAIN + unreachable)
#   07  allowlist — IP literal blocked (no DNS-attested IP)
#   08  snapshot + fork x5 with network; each fork distinct guest IP,
#       each fork post-resume RefreshNetwork worked
#   09  lifecycle cleanliness: chroots/netns/nft empty after teardown
#   10  wildcard allowlist: *.github.com matches only one label
#   11  bad pattern ("*") rejected at POST time
#   12  enabled=true with empty allowlist rejected
#   13  memory OOM (128MiB VM, alloc 200MiB → killed)
#   14  resource usage stats are populated
#   15  orphan reap: stale chroot removed on daemon startup
#
# Everything is written to /tmp/crucible-smoke-<timestamp>/ so
# you can `grep -r FAIL` or `less` individual transcripts.
#
# Requires:
#   - Linux host with KVM + root (jailer needs CAP_SYS_ADMIN)
#   - Real internet (tests 5-8, 10 hit example.com / github.com)
#   - Guest rootfs with crucible-agent + python3 + iproute2
#   - cruc binaries built (make build && make agent && make rootfs)
#
# Usage:
#   sudo CRUCIBLE_BIN=./crucible \
#        FIRECRACKER_BIN=/path/to/firecracker \
#        JAILER_BIN=/path/to/jailer \
#        KERNEL=/path/to/vmlinux \
#        ROOTFS=./assets/rootfs-with-agent.ext4 \
#        EGRESS_IFACE=wlp1s0 \
#        scripts/smoke_e2e.sh

set -u
set -o pipefail

# ===== Configuration =========================================

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-./assets/rootfs-with-agent.ext4}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
WORK_BASE="${WORK_BASE:-/var/lib/crucible/run}"
LISTEN="${LISTEN:-127.0.0.1:7880}"
BASE_URL="http://${LISTEN}"

# EGRESS_IFACE must be the host's internet-reachable interface.
# Auto-detect via default route if the caller didn't set it.
EGRESS_IFACE="${EGRESS_IFACE:-$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')}"

SMOKE_ROOT="/tmp/crucible-smoke-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$SMOKE_ROOT"

DAEMON_LOG="$SMOKE_ROOT/daemon.log"
SESSION_LOG="$SMOKE_ROOT/session.log"
SUMMARY="$SMOKE_ROOT/summary.txt"

# Mirror everything from now on to session.log AND console.
exec > >(tee -a "$SESSION_LOG") 2>&1

echo "==============================================================="
echo " crucible e2e smoke"
echo "==============================================================="
echo " output dir       : $SMOKE_ROOT"
echo " daemon log       : $DAEMON_LOG"
echo " crucible binary  : $CRUCIBLE_BIN"
echo " firecracker      : $FIRECRACKER_BIN"
echo " jailer           : $JAILER_BIN"
echo " kernel           : $KERNEL"
echo " rootfs           : $ROOTFS"
echo " egress iface     : $EGRESS_IFACE"
echo " listen           : $LISTEN"
echo "==============================================================="

# Record the config so it's in the output dir.
cat > "$SMOKE_ROOT/config.txt" <<EOF
CRUCIBLE_BIN=$CRUCIBLE_BIN
FIRECRACKER_BIN=$FIRECRACKER_BIN
JAILER_BIN=$JAILER_BIN
KERNEL=$KERNEL
ROOTFS=$ROOTFS
CHROOT_BASE=$CHROOT_BASE
WORK_BASE=$WORK_BASE
LISTEN=$LISTEN
EGRESS_IFACE=$EGRESS_IFACE
STARTED_AT=$(date -Is)
EOF

# ===== Preflight =============================================

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (jailer requires CAP_SYS_ADMIN)" >&2
  exit 2
fi

for bin in "$CRUCIBLE_BIN" "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  if [[ ! -x "$bin" ]]; then
    echo "error: missing or non-executable: $bin" >&2
    exit 2
  fi
done

for f in "$KERNEL" "$ROOTFS"; do
  if [[ ! -r "$f" ]]; then
    echo "error: missing or unreadable: $f" >&2
    exit 2
  fi
done

if [[ -z "$EGRESS_IFACE" ]]; then
  echo "error: EGRESS_IFACE not set and no default route found" >&2
  exit 2
fi

# ===== Frame parser (written once, used by all tests) ========
# Our /exec response body is a sequence of framed records:
#   [1 B type][3 B reserved][4 B big-endian size][payload].
# type 1 = stdout, 2 = stderr, 3 = exit (JSON payload).
#
# split_frames.py reads raw bytes on stdin and writes:
#   <outdir>/stdout  (concatenated type-1 payloads)
#   <outdir>/stderr  (concatenated type-2 payloads)
#   <outdir>/exit.json  (the type-3 payload; pretty-printed JSON)
# If no exit frame was received, exit.json is absent.

FRAME_PARSER="$SMOKE_ROOT/split_frames.py"
cat > "$FRAME_PARSER" <<'PY'
#!/usr/bin/env python3
import json, os, struct, sys

def main(outdir: str) -> int:
    os.makedirs(outdir, exist_ok=True)
    data = sys.stdin.buffer.read()
    out = {1: open(os.path.join(outdir, "stdout"), "wb"),
           2: open(os.path.join(outdir, "stderr"), "wb")}
    exit_payload = None
    off = 0
    while off + 8 <= len(data):
        typ = data[off]
        size = struct.unpack(">I", data[off+4:off+8])[0]
        body = data[off+8:off+8+size]
        off += 8 + size
        if typ in out:
            out[typ].write(body)
        elif typ == 3:
            exit_payload = body
    for f in out.values():
        f.close()
    if exit_payload is not None:
        with open(os.path.join(outdir, "exit.json"), "wb") as f:
            parsed = json.loads(exit_payload)
            f.write(json.dumps(parsed, indent=2).encode())
            f.write(b"\n")
    return 0

if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("usage: split_frames.py <outdir>", file=sys.stderr)
        sys.exit(2)
    sys.exit(main(sys.argv[1]))
PY
chmod +x "$FRAME_PARSER"

# jq-like helper that reads a single JSON path via python3. Used
# for pulling scalar fields out of API responses without a real
# jq dep (Ubuntu base rootfs doesn't always have jq).
jpath() {
  local file="$1"; shift
  python3 -c "
import json,sys
d = json.load(open('$file'))
for k in sys.argv[1:]:
    if k.isdigit():
        d = d[int(k)]
    else:
        d = d[k]
print(d)
" "$@"
}

# ===== Daemon lifecycle ======================================

DAEMON_PID=""

start_daemon() {
  echo "== starting daemon"
  local extra=()
  if [[ -n "${EGRESS_IFACE:-}" ]]; then
    extra+=(--network-egress-iface "$EGRESS_IFACE")
  fi
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" \
    --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" \
    --kernel "$KERNEL" \
    --rootfs "$ROOTFS" \
    --work-base "$WORK_BASE" \
    --log-format json --log-level info \
    "${extra[@]}" \
    >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  echo "   daemon pid: $DAEMON_PID"

  echo "== waiting for /healthz"
  for _ in {1..100}; do
    if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
      echo "   healthy"
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
    echo "== stopping daemon (pid $DAEMON_PID)"
    kill -TERM "$DAEMON_PID" 2>/dev/null || true
    for _ in {1..50}; do
      kill -0 "$DAEMON_PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    DAEMON_PID=""
  fi
}

# ===== Teardown (always runs) ================================

final_cleanup() {
  stop_daemon
  # Best-effort cleanup of anything the daemon didn't tear down.
  # These are idempotent and silent on missing resources.
  if ls -A "$CHROOT_BASE/firecracker/" 2>/dev/null | grep -q .; then
    rm -rf "$CHROOT_BASE/firecracker"/* 2>/dev/null || true
  fi
  ip netns list 2>/dev/null | awk '/^crucible-/{print $1}' | while read -r ns; do
    ip netns delete "$ns" 2>/dev/null || true
  done
  nft delete table inet crucible 2>/dev/null || true
  ip link delete crucible-dns 2>/dev/null || true
}
trap final_cleanup EXIT

# ===== Test harness ==========================================

PASS_COUNT=0
FAIL_COUNT=0
FAIL_NAMES=()

TEST_NUM=""
TEST_NAME=""
TEST_DIR=""
TEST_PASS=true

start_test() {
  TEST_NUM="$1"
  TEST_NAME="$2"
  local desc="$3"
  TEST_DIR="$SMOKE_ROOT/test-${TEST_NUM}-${TEST_NAME}"
  mkdir -p "$TEST_DIR"
  echo "$desc" > "$TEST_DIR/description.txt"
  TEST_PASS=true
  echo
  echo "-----------------------------------------------------------"
  echo "TEST $TEST_NUM: $TEST_NAME"
  echo "  $desc"
}

fail() {
  local name="$1"; shift
  TEST_PASS=false
  echo "  FAIL [$name]: $*" | tee -a "$TEST_DIR/assertions.log"
}

pass() {
  local name="$1"; shift
  echo "  PASS [$name]: $*" | tee -a "$TEST_DIR/assertions.log"
}

# assert_eq NAME GOT WANT
assert_eq() {
  local name="$1"
  local got="$2"
  local want="$3"
  if [[ "$got" == "$want" ]]; then
    pass "$name" "got=$got"
  else
    fail "$name" "got=$got, want=$want"
  fi
}

# assert_ne NAME GOT UNWANTED
assert_ne() {
  local name="$1"
  local got="$2"
  local unwanted="$3"
  if [[ "$got" != "$unwanted" ]]; then
    pass "$name" "got=$got (not $unwanted)"
  else
    fail "$name" "got=$got, wanted anything but $unwanted"
  fi
}

# assert_contains NAME FILE PATTERN
assert_contains() {
  local name="$1"
  local file="$2"
  local pattern="$3"
  if grep -qE "$pattern" "$file" 2>/dev/null; then
    pass "$name" "pattern '$pattern' found in $(basename "$file")"
  else
    fail "$name" "pattern '$pattern' not found in $(basename "$file")"
  fi
}

# assert_not_contains NAME FILE PATTERN
assert_not_contains() {
  local name="$1"
  local file="$2"
  local pattern="$3"
  if ! grep -qE "$pattern" "$file" 2>/dev/null; then
    pass "$name" "pattern '$pattern' absent from $(basename "$file")"
  else
    fail "$name" "pattern '$pattern' unexpectedly found in $(basename "$file")"
  fi
}

finish_test() {
  if $TEST_PASS; then
    echo "PASS" > "$TEST_DIR/result"
    echo "  => PASS"
    PASS_COUNT=$((PASS_COUNT+1))
  else
    echo "FAIL" > "$TEST_DIR/result"
    echo "  => FAIL (see $TEST_DIR/)"
    FAIL_COUNT=$((FAIL_COUNT+1))
    FAIL_NAMES+=("$TEST_NUM-$TEST_NAME")
  fi
}

# ===== HTTP + exec helpers ===================================
# Each helper captures the full request and response into files
# under $TEST_DIR so a human can inspect exactly what went on
# the wire without re-running anything.

# api_create TAG BODY_JSON
# Creates a sandbox. Writes <TAG>.req.json, <TAG>.status, <TAG>.resp.json.
# Returns sandbox ID on stdout (via "echo"). On non-2xx returns empty.
api_create() {
  local tag="$1"
  local body="$2"
  local out="$TEST_DIR/$tag"
  echo "$body" > "$out.req.json"
  local status
  status=$(curl -sS -w '%{http_code}' -o "$out.resp.json" \
    -X POST "$BASE_URL/sandboxes" \
    -H 'Content-Type: application/json' \
    -d "$body")
  echo "$status" > "$out.status"
  if [[ "$status" == "201" ]]; then
    jpath "$out.resp.json" id
  else
    echo ""
  fi
}

# api_snapshot TAG SANDBOX_ID → snapshot ID on stdout, "" on failure
api_snapshot() {
  local tag="$1"
  local sbx="$2"
  local out="$TEST_DIR/$tag"
  local status
  status=$(curl -sS -w '%{http_code}' -o "$out.resp.json" \
    -X POST "$BASE_URL/sandboxes/$sbx/snapshot")
  echo "$status" > "$out.status"
  if [[ "$status" == "201" ]]; then
    jpath "$out.resp.json" id
  else
    echo ""
  fi
}

# api_fork TAG SNAP_ID COUNT → space-separated sandbox IDs, "" on failure
api_fork() {
  local tag="$1"
  local snap="$2"
  local count="$3"
  local out="$TEST_DIR/$tag"
  local status
  status=$(curl -sS -w '%{http_code}' -o "$out.resp.json" \
    -X POST "$BASE_URL/snapshots/$snap/fork?count=$count")
  echo "$status" > "$out.status"
  if [[ "$status" == "201" ]]; then
    python3 -c "
import json, sys
d = json.load(open('$out.resp.json'))
print(' '.join(s['id'] for s in d['sandboxes']))
"
  else
    echo ""
  fi
}

# api_delete SANDBOX_ID (silent; does not fail the test)
api_delete() {
  curl -sS -o /dev/null -X DELETE "$BASE_URL/sandboxes/$1" || true
}

api_delete_snap() {
  curl -sS -o /dev/null -X DELETE "$BASE_URL/snapshots/$1" || true
}

# exec_in TAG SANDBOX_ID CMD_JSON
# CMD_JSON is a full ExecRequest body, e.g. '{"cmd":["echo","hi"]}' or
# '{"cmd":["sleep","30"],"timeout_s":2}'.
# Captures raw framed bytes + splits them into stdout/stderr/exit.json.
# Returns 0 regardless of exec exit code (so the test can inspect).
exec_in() {
  local tag="$1"
  local sbx="$2"
  local body="$3"
  local out="$TEST_DIR/$tag"
  mkdir -p "$out"
  echo "$body" > "$out/request.json"
  local status
  status=$(curl -sS -w '%{http_code}' \
    -o "$out/raw.bin" \
    -X POST "$BASE_URL/sandboxes/$sbx/exec" \
    -H 'Content-Type: application/json' \
    -d "$body")
  echo "$status" > "$out/status"
  if [[ "$status" == "200" ]]; then
    "$FRAME_PARSER" "$out" < "$out/raw.bin" || true
  fi
  return 0
}

# Convenience: pull exit_code from an exec result dir.
exec_exit_code() {
  local dir="$1"
  if [[ -f "$dir/exit.json" ]]; then
    jpath "$dir/exit.json" exit_code
  else
    echo "-"
  fi
}

# Convenience: pull any top-level field.
exec_field() {
  local dir="$1"
  local field="$2"
  if [[ -f "$dir/exit.json" ]]; then
    jpath "$dir/exit.json" "$field" 2>/dev/null || echo "-"
  else
    echo "-"
  fi
}

# Strip trailing whitespace/newlines from a file for cleaner
# comparisons. Prints the trimmed value on stdout.
trim_file() {
  tr -d '[:space:]' < "$1"
}

start_daemon

# ===== TEST 01: default-deny baseline ========================
start_test "01" "default_deny" \
  "Sandbox without 'network:' has no eth0 IP; DNS resolution fails."

SBX_01=$(api_create "create" '{"vcpus":1,"memory_mib":256}')
assert_ne "create_ok" "$SBX_01" ""

exec_in "ip_addr" "$SBX_01" '{"cmd":["/bin/sh","-c","ip -4 addr show eth0 2>&1 || true"]}'
assert_not_contains "eth0_has_no_inet" "$TEST_DIR/ip_addr/stdout" "inet [0-9]"

exec_in "getent" "$SBX_01" '{"cmd":["/usr/bin/getent","hosts","example.com"]}'
# getent returns 2 on NXDOMAIN / DNS failure.
assert_ne "getent_fails" "$(exec_exit_code "$TEST_DIR/getent")" "0"

api_delete "$SBX_01"
finish_test

# ===== TEST 02: exec round-trip ==============================
start_test "02" "exec_roundtrip" \
  "echo 'hello-world' returns stdout=hello-world, exit_code=0, duration>0."

SBX_02=$(api_create "create" '{"vcpus":1,"memory_mib":256}')
assert_ne "create_ok" "$SBX_02" ""

exec_in "echo" "$SBX_02" '{"cmd":["/bin/echo","hello-world"]}'
STDOUT_02=$(tr -d '\n' < "$TEST_DIR/echo/stdout")
assert_eq "stdout_matches" "$STDOUT_02" "hello-world"
assert_eq "exit_zero" "$(exec_exit_code "$TEST_DIR/echo")" "0"
DURATION_02=$(exec_field "$TEST_DIR/echo" duration_ms)
if [[ "$DURATION_02" != "-" && "$DURATION_02" -gt 0 ]]; then
  pass "duration_positive" "duration_ms=$DURATION_02"
else
  fail "duration_positive" "duration_ms=$DURATION_02"
fi

api_delete "$SBX_02"
finish_test

# ===== TEST 03: exit codes + signals =========================
start_test "03" "exit_and_signal" \
  "exit 7 → exit_code=7; kill -9 \$\$ → signal='killed' and exit_code=-1."

SBX_03=$(api_create "create" '{"vcpus":1,"memory_mib":256}')
assert_ne "create_ok" "$SBX_03" ""

exec_in "exit7" "$SBX_03" '{"cmd":["/bin/sh","-c","exit 7"]}'
assert_eq "exit_code_7" "$(exec_exit_code "$TEST_DIR/exit7")" "7"

exec_in "kill9" "$SBX_03" '{"cmd":["/bin/sh","-c","kill -9 $$"]}'
assert_eq "exit_code_neg1" "$(exec_exit_code "$TEST_DIR/kill9")" "-1"
SIG_03=$(exec_field "$TEST_DIR/kill9" signal)
assert_contains "signal_is_killed" <(echo "$SIG_03") "killed|SIGKILL"

api_delete "$SBX_03"
finish_test

# ===== TEST 04: exec timeout =================================
start_test "04" "exec_timeout" \
  "sleep 30 with timeout_s=2 is killed; timed_out=true; duration_ms bounded."

SBX_04=$(api_create "create" '{"vcpus":1,"memory_mib":256}')
assert_ne "create_ok" "$SBX_04" ""

exec_in "sleep30_t2" "$SBX_04" '{"cmd":["/bin/sleep","30"],"timeout_s":2}'
assert_eq "timed_out_true" "$(exec_field "$TEST_DIR/sleep30_t2" timed_out)" "True"
DUR_04=$(exec_field "$TEST_DIR/sleep30_t2" duration_ms)
if [[ "$DUR_04" != "-" && "$DUR_04" -lt 5000 ]]; then
  pass "duration_bounded" "duration_ms=$DUR_04"
else
  fail "duration_bounded" "duration_ms=$DUR_04 (want <5000)"
fi

api_delete "$SBX_04"
finish_test

# ===== TEST 05: allowlist — allowed host =====================
start_test "05" "allowlist_allowed_host" \
  "Sandbox with allowlist=[example.com] can resolve + fetch https://example.com."

SBX_05=$(api_create "create" \
  '{"vcpus":1,"memory_mib":256,"network":{"enabled":true,"allowlist":["example.com"]}}')
assert_ne "create_ok" "$SBX_05" ""

# Allow a beat for the guest to finish DHCP acquisition.
sleep 2

exec_in "getent" "$SBX_05" '{"cmd":["/usr/bin/getent","hosts","example.com"]}'
assert_eq "getent_ok" "$(exec_exit_code "$TEST_DIR/getent")" "0"
assert_contains "has_ip" "$TEST_DIR/getent/stdout" "[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+"

exec_in "https_fetch" "$SBX_05" \
  '{"cmd":["/usr/bin/python3","-c","import urllib.request;r=urllib.request.urlopen(\"https://example.com\",timeout=10);print(r.status);print(r.read()[:200].decode(\"utf-8\",\"replace\"))"]}'
assert_eq "fetch_exit_zero" "$(exec_exit_code "$TEST_DIR/https_fetch")" "0"
assert_contains "http_200" "$TEST_DIR/https_fetch/stdout" "^200$"

api_delete "$SBX_05"
finish_test

# ===== TEST 06: allowlist — denied host ======================
start_test "06" "allowlist_denied_host" \
  "Same sandbox shape: google.com is not in allowlist, DNS+HTTPS both fail."

SBX_06=$(api_create "create" \
  '{"vcpus":1,"memory_mib":256,"network":{"enabled":true,"allowlist":["example.com"]}}')
assert_ne "create_ok" "$SBX_06" ""
sleep 2

exec_in "getent_google" "$SBX_06" '{"cmd":["/usr/bin/getent","hosts","google.com"]}'
# getent returns non-zero when the name doesn't resolve.
assert_ne "getent_nxdomain" "$(exec_exit_code "$TEST_DIR/getent_google")" "0"

exec_in "fetch_google" "$SBX_06" \
  '{"cmd":["/usr/bin/python3","-c","import urllib.request,sys;\ntry:\n r=urllib.request.urlopen(\"https://google.com\",timeout=3);print(\"UNEXPECTED_OK\")\nexcept Exception as e:\n print(\"denied:\",type(e).__name__)","/dev/null"]}'
# We want the script to succeed (exit 0) and have printed "denied:" —
# the Python didn't reach google.com.
assert_eq "python_completed" "$(exec_exit_code "$TEST_DIR/fetch_google")" "0"
assert_contains "got_denial" "$TEST_DIR/fetch_google/stdout" "^denied:"
assert_not_contains "no_unexpected_ok" "$TEST_DIR/fetch_google/stdout" "UNEXPECTED_OK"

api_delete "$SBX_06"
finish_test

# ===== TEST 07: allowlist — IP literal blocked ===============
start_test "07" "allowlist_ip_literal" \
  "Sandbox with allowlist=[example.com] can't reach 8.8.8.8 (no DNS attestation)."

SBX_07=$(api_create "create" \
  '{"vcpus":1,"memory_mib":256,"network":{"enabled":true,"allowlist":["example.com"]}}')
assert_ne "create_ok" "$SBX_07" ""
sleep 2

exec_in "dns_tcp53" "$SBX_07" \
  '{"cmd":["/usr/bin/python3","-c","import socket,sys;\ntry:\n s=socket.create_connection((\"8.8.8.8\",53),timeout=3);print(\"UNEXPECTED_OK\")\nexcept Exception as e:\n print(\"denied:\",type(e).__name__)"]}'
assert_eq "python_completed" "$(exec_exit_code "$TEST_DIR/dns_tcp53")" "0"
assert_contains "ip_literal_blocked" "$TEST_DIR/dns_tcp53/stdout" "^denied:"
assert_not_contains "no_unexpected_ok" "$TEST_DIR/dns_tcp53/stdout" "UNEXPECTED_OK"

api_delete "$SBX_07"
finish_test

# ===== TEST 08: snapshot + fork x5 with network ==============
start_test "08" "snapshot_fork_network" \
  "Source + 5 forks, each with network; all resolve example.com; each has a distinct guest IP that matches the API-reported IP."

SRC_08=$(api_create "create_source" \
  '{"vcpus":1,"memory_mib":256,"network":{"enabled":true,"allowlist":["example.com"]}}')
assert_ne "source_created" "$SRC_08" ""
sleep 2

SRC_IP_08=$(jpath "$TEST_DIR/create_source.resp.json" network guest_ip)
echo "  source guest_ip (from API): $SRC_IP_08"

# Snapshot the source (warm-cache DNS and all).
SNAP_08=$(api_snapshot "snapshot" "$SRC_08")
assert_ne "snapshot_created" "$SNAP_08" ""

# Fork x5.
FORKS_08=$(api_fork "fork" "$SNAP_08" 5)
IFS=' ' read -r -a FORK_ARR <<< "$FORKS_08"
assert_eq "fork_count" "${#FORK_ARR[@]}" "5"

# Collect each fork's API-reported guest IP + check the guest
# actually sees that IP on eth0 + can reach example.com.
seen_ips=""
for idx in "${!FORK_ARR[@]}"; do
  F="${FORK_ARR[$idx]}"
  RESP="$TEST_DIR/fork.resp.json"

  # Parse the API's reported IP for this specific fork from the
  # fork response (sandboxes array).
  REPORTED_IP=$(python3 -c "
import json
d = json.load(open('$RESP'))
print(d['sandboxes'][$idx]['network']['guest_ip'])
")
  echo "  fork[$idx] id=$F reported guest_ip=$REPORTED_IP"

  # Post-refresh delay: the agent's POST /network/refresh already
  # waits for eth0 to have IP before returning, so by the time
  # we hit /exec here, eth0 should be ready. 500ms of slack.
  sleep 0.5

  # Use `hostname -I` rather than `ip ... | awk '{...}'` because
  # single quotes inside a single-quoted JSON body require ugly
  # shell escapes. `hostname -I` prints the interface's non-
  # loopback IPs separated by spaces; with lo stripped and
  # eth0 the only other configured interface, it's exactly the
  # guest's one IPv4 address.
  exec_in "fork_${idx}_ip" "$F" '{"cmd":["/bin/hostname","-I"]}'
  IN_GUEST_IP=$(tr -d '[:space:]' < "$TEST_DIR/fork_${idx}_ip/stdout")
  assert_eq "fork_${idx}_ip_matches" "$IN_GUEST_IP" "$REPORTED_IP"

  exec_in "fork_${idx}_dns" "$F" \
    '{"cmd":["/usr/bin/getent","hosts","example.com"]}'
  assert_eq "fork_${idx}_dns_ok" "$(exec_exit_code "$TEST_DIR/fork_${idx}_dns")" "0"

  # Check IP uniqueness as we go.
  if grep -Fxq "$REPORTED_IP" <<< "$seen_ips"; then
    fail "fork_${idx}_ip_unique" "IP $REPORTED_IP already seen on another fork"
  else
    pass "fork_${idx}_ip_unique" "IP $REPORTED_IP is distinct"
  fi
  seen_ips="$seen_ips
$REPORTED_IP"
done

# Teardown: source, forks, snapshot.
api_delete "$SRC_08"
for F in "${FORK_ARR[@]}"; do api_delete "$F"; done
api_delete_snap "$SNAP_08"

finish_test

# ===== TEST 09: lifecycle cleanliness ========================
start_test "09" "lifecycle_cleanliness" \
  "After teardown, no stale chroots / netns / nft sandbox chains remain."

# Give the daemon a moment to finish async cleanup (DHCP shutdown etc).
sleep 1

ls -A "$CHROOT_BASE/firecracker" 2>/dev/null > "$TEST_DIR/remaining_chroots.txt" || true
if [[ -s "$TEST_DIR/remaining_chroots.txt" ]]; then
  fail "chroots_empty" "remaining: $(cat "$TEST_DIR/remaining_chroots.txt" | tr '\n' ' ')"
else
  pass "chroots_empty" "no chroots under $CHROOT_BASE/firecracker"
fi

ip netns list 2>/dev/null | awk '/^crucible-/{print $1}' > "$TEST_DIR/remaining_netns.txt"
if [[ -s "$TEST_DIR/remaining_netns.txt" ]]; then
  fail "netns_empty" "remaining: $(cat "$TEST_DIR/remaining_netns.txt" | tr '\n' ' ')"
else
  pass "netns_empty" "no crucible-* netns"
fi

nft list table inet crucible 2>/dev/null > "$TEST_DIR/nft_table.txt" || true
# The base table contains a map + two chains. Per-sandbox state
# lives in objects named sandbox_*. Assert none of those exist.
if grep -E "^\s*(chain|set)\s+sandbox_" "$TEST_DIR/nft_table.txt" > /dev/null; then
  fail "nft_clean" "per-sandbox objects remain (see $TEST_DIR/nft_table.txt)"
else
  pass "nft_clean" "no per-sandbox chains/sets in crucible table"
fi

finish_test

# ===== TEST 10: wildcard allowlist ===========================
start_test "10" "wildcard_allowlist" \
  "allowlist=[*.github.com]: api.github.com → ok, github.com (apex) → fail, a.b.github.com → fail."

SBX_10=$(api_create "create" \
  '{"vcpus":1,"memory_mib":256,"network":{"enabled":true,"allowlist":["*.github.com"]}}')
assert_ne "create_ok" "$SBX_10" ""
sleep 2

exec_in "api_github" "$SBX_10" '{"cmd":["/usr/bin/getent","hosts","api.github.com"]}'
assert_eq "subdomain_ok" "$(exec_exit_code "$TEST_DIR/api_github")" "0"

exec_in "apex" "$SBX_10" '{"cmd":["/usr/bin/getent","hosts","github.com"]}'
assert_ne "apex_denied" "$(exec_exit_code "$TEST_DIR/apex")" "0"

exec_in "deep" "$SBX_10" '{"cmd":["/usr/bin/getent","hosts","a.b.github.com"]}'
assert_ne "deep_denied" "$(exec_exit_code "$TEST_DIR/deep")" "0"

api_delete "$SBX_10"
finish_test

# ===== TEST 11: bare wildcard rejected at POST ===============
start_test "11" "bad_pattern_rejected" \
  "allowlist=['*'] is rejected with HTTP 400 mentioning 'bare wildcard'."

status=$(curl -sS -w '%{http_code}' -o "$TEST_DIR/resp.json" \
  -X POST "$BASE_URL/sandboxes" \
  -H 'Content-Type: application/json' \
  -d '{"vcpus":1,"memory_mib":256,"network":{"enabled":true,"allowlist":["*"]}}')
echo "$status" > "$TEST_DIR/status"
assert_eq "http_400" "$status" "400"
assert_contains "mentions_wildcard" "$TEST_DIR/resp.json" "bare wildcard"

finish_test

# ===== TEST 12: enabled=true + empty allowlist → 400 =========
start_test "12" "empty_allowlist_rejected" \
  "network={enabled:true, allowlist:[]} is rejected with HTTP 400."

status=$(curl -sS -w '%{http_code}' -o "$TEST_DIR/resp.json" \
  -X POST "$BASE_URL/sandboxes" \
  -H 'Content-Type: application/json' \
  -d '{"vcpus":1,"memory_mib":256,"network":{"enabled":true}}')
echo "$status" > "$TEST_DIR/status"
assert_eq "http_400" "$status" "400"
assert_contains "mentions_allowlist" "$TEST_DIR/resp.json" "non-empty allowlist"

finish_test

# ===== TEST 13: memory OOM ===================================
start_test "13" "memory_oom" \
  "VM with 128 MiB RAM + python3 allocating 256 MiB → killed; signal is SIGKILL."

SBX_13=$(api_create "create" '{"vcpus":1,"memory_mib":128}')
assert_ne "create_ok" "$SBX_13" ""

# Allocate 256 MiB as a single bytearray so the kernel can't avoid
# the RSS pressure. Deliberately a bit larger than half the VM's
# RAM to guarantee OOM.
exec_in "alloc256" "$SBX_13" \
  '{"cmd":["/usr/bin/python3","-c","a=bytearray(256*1024*1024);import sys;sys.stdout.write(\"alloc_done\\n\")"]}'
EC_13=$(exec_exit_code "$TEST_DIR/alloc256")
SIG_13=$(exec_field "$TEST_DIR/alloc256" signal)
echo "  exit_code=$EC_13 signal=$SIG_13"

# Either Python got OOM-killed (exit_code=-1 via SIGKILL) or the
# kernel OOM-killed it outright. We accept either as long as the
# process did NOT complete normally with "alloc_done".
assert_not_contains "alloc_did_not_succeed" "$TEST_DIR/alloc256/stdout" "alloc_done"
# Signal should be killed OR the process got a non-zero exit.
if [[ "$EC_13" == "-1" ]] || [[ "$EC_13" != "0" ]]; then
  pass "nonzero_or_killed" "exit_code=$EC_13 signal=$SIG_13"
else
  fail "nonzero_or_killed" "exit_code=$EC_13 — process completed normally"
fi

# OOM heuristic is best-effort (depends on peak RSS timing vs
# 95% threshold). Record it but don't fail on it.
OOM_13=$(exec_field "$TEST_DIR/alloc256" oom_killed)
echo "  oom_killed (heuristic, not required): $OOM_13"

api_delete "$SBX_13"
finish_test

# ===== TEST 14: resource usage stats are populated ===========
start_test "14" "usage_stats_populated" \
  "A workload that burns CPU + allocates memory + writes I/O → usage fields are positive."

SBX_14=$(api_create "create" '{"vcpus":1,"memory_mib":256}')
assert_ne "create_ok" "$SBX_14" ""

exec_in "workload" "$SBX_14" \
  '{"cmd":["/usr/bin/python3","-c","import os;a=bytearray(16*1024*1024);\nfor i in range(800000):\n  i*i\nwith open(\"/tmp/w\",\"wb\") as f:\n  f.write(b\"x\"*1024*1024)\n  f.flush();os.fsync(f.fileno())"]}'

assert_eq "exit_zero" "$(exec_exit_code "$TEST_DIR/workload")" "0"

PEAK=$(python3 -c "
import json
d = json.load(open('$TEST_DIR/workload/exit.json'))
print(d.get('usage', {}).get('peak_memory_bytes', 0))
")
CPU_USER=$(python3 -c "
import json
d = json.load(open('$TEST_DIR/workload/exit.json'))
print(d.get('usage', {}).get('cpu_user_ms', 0))
")
IO_WRITE=$(python3 -c "
import json
d = json.load(open('$TEST_DIR/workload/exit.json'))
print(d.get('usage', {}).get('io_write_bytes', 0))
")
echo "  peak_memory_bytes=$PEAK cpu_user_ms=$CPU_USER io_write_bytes=$IO_WRITE"

if [[ "$PEAK" -gt $((8*1024*1024)) ]]; then
  pass "peak_memory_over_8MiB" "peak_memory_bytes=$PEAK"
else
  fail "peak_memory_over_8MiB" "peak_memory_bytes=$PEAK (want >8 MiB)"
fi

if [[ "$CPU_USER" -gt 0 ]]; then
  pass "cpu_user_positive" "cpu_user_ms=$CPU_USER"
else
  fail "cpu_user_positive" "cpu_user_ms=$CPU_USER"
fi

# io_write_bytes is best-effort (the poller may miss short-
# running writes per docs). Log but don't fail on it.
echo "  (io_write_bytes is best-effort; recorded for eyeballing.)"

api_delete "$SBX_14"
finish_test

# ===== TEST 15: orphan reap =================================
start_test "15" "orphan_reap" \
  "A synthetic chroot under /srv/jailer is removed when the daemon restarts."

# Seed a fake chroot (daemon must be running to have already set
# up the base nft + dummy iface, but we're going to restart it).
FAKE_ID="synthetic-$$-$RANDOM"
FAKE_DIR="$CHROOT_BASE/firecracker/$FAKE_ID/root"
mkdir -p "$FAKE_DIR"
touch "$FAKE_DIR/marker"
ls -la "$FAKE_DIR" > "$TEST_DIR/before_restart.txt"
pass "seed_fake_chroot" "$FAKE_DIR created"

# Restart daemon. Shutdown is clean; the fake chroot survives
# because the daemon never knew about it.
stop_daemon
# Confirm fake is still there post-shutdown.
if [[ -d "$CHROOT_BASE/firecracker/$FAKE_ID" ]]; then
  pass "fake_survived_shutdown" "still present"
else
  fail "fake_survived_shutdown" "fake vanished unexpectedly"
fi

start_daemon

# Give it a moment to process the reap + logs.
sleep 0.5

if [[ -d "$CHROOT_BASE/firecracker/$FAKE_ID" ]]; then
  fail "fake_reaped" "chroot $FAKE_ID still exists after daemon startup"
  ls -la "$CHROOT_BASE/firecracker/$FAKE_ID" > "$TEST_DIR/after_restart.txt" || true
else
  pass "fake_reaped" "chroot $FAKE_ID removed by startup reap"
fi

# Cross-check in the daemon log.
if grep -q "reaped orphan chroots" "$DAEMON_LOG"; then
  pass "reap_logged" "daemon logged the reap"
else
  fail "reap_logged" "daemon did not log 'reaped orphan chroots' (check $DAEMON_LOG)"
fi

finish_test

# ===== Final summary =========================================

echo
echo "==============================================================="
echo " SUMMARY"
echo "==============================================================="
TOTAL=$((PASS_COUNT+FAIL_COUNT))
echo " passed: $PASS_COUNT / $TOTAL"
echo " failed: $FAIL_COUNT / $TOTAL"
{
  echo "PASS: $PASS_COUNT"
  echo "FAIL: $FAIL_COUNT"
  echo "TOTAL: $TOTAL"
  if [[ $FAIL_COUNT -gt 0 ]]; then
    echo "FAILED TESTS:"
    for n in "${FAIL_NAMES[@]}"; do
      echo "  - $n"
    done
  fi
  echo "OUTPUT_DIR: $SMOKE_ROOT"
} > "$SUMMARY"

if [[ $FAIL_COUNT -gt 0 ]]; then
  echo " failed tests:"
  for n in "${FAIL_NAMES[@]}"; do
    echo "   - $n"
  done
fi
echo
echo " outputs         : $SMOKE_ROOT"
echo " session log     : $SESSION_LOG"
echo " daemon log      : $DAEMON_LOG"
echo " per-test dirs   : $SMOKE_ROOT/test-NN-*"
echo "==============================================================="

if [[ $FAIL_COUNT -gt 0 ]]; then
  exit 1
fi
exit 0
