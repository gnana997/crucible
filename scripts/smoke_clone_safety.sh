#!/usr/bin/env bash
#
# End-to-end clone-safety check.
#
# What this proves, using one snapshot forked into two children:
#   1. machine-id: each fork's /etc/machine-id differs from the
#      source's and from the other fork's (agent rotation).
#   2. Kernel CRNG: /proc/sys/kernel/random/uuid differs across forks.
#   3. Straddling process: a process started BEFORE the snapshot that
#      reads /dev/urandom AFTER resume gets different bytes in each
#      fork — the already-running-code case VMGenID exists for.
#   4. VMGenID actually fired: dmesg in a fork contains the kernel's
#      "crng reseeded due to virtual machine fork" notice. This is a
#      REQUIREMENT — if it fails, the guest kernel lacks
#      CONFIG_VMGENID and that is a gap to fix, not a warning.
#   5. Fork identity plumbing: hostname == sandbox ID and
#      /run/crucible/fork-id == sandbox ID inside each fork.
#
# Requires root because jailer needs CAP_SYS_ADMIN + privilege drop.
# Overridable via env vars; sensible defaults below.
#
# Usage:
#   sudo CRUCIBLE_BIN=./crucible \
#        FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        ROOTFS=/var/lib/crucible/rootfs.ext4 \
#        scripts/smoke_clone_safety.sh
#
# Exits 0 on success, non-zero on any failure.

set -euo pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-/var/lib/crucible/rootfs.ext4}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
WORK_BASE="${WORK_BASE:-/var/lib/crucible/run}"
LISTEN="${LISTEN:-127.0.0.1:7880}"   # own port; don't collide with other smokes
BASE_URL="http://${LISTEN}"

# The straddling process sleeps this long (guest-side) before reading
# /dev/urandom. Must comfortably outlast exec+snapshot+fork so the
# read happens after each fork resumes.
STRADDLE_SLEEP=20

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (jailer requires CAP_SYS_ADMIN)" >&2
  exit 1
fi

for bin in "$CRUCIBLE_BIN" "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  if [[ ! -x "$bin" ]]; then
    echo "error: missing or non-executable: $bin" >&2
    exit 1
  fi
done
for f in "$KERNEL" "$ROOTFS"; do
  if [[ ! -r "$f" ]]; then
    echo "error: missing or unreadable: $f" >&2
    exit 1
  fi
done

DAEMON_LOG=$(mktemp)

echo "== starting daemon (jailer mode) on $LISTEN"
"$CRUCIBLE_BIN" daemon \
  --listen "$LISTEN" \
  --firecracker-bin "$FIRECRACKER_BIN" \
  --jailer-bin "$JAILER_BIN" \
  --chroot-base "$CHROOT_BASE" \
  --kernel "$KERNEL" \
  --rootfs "$ROOTFS" \
  --work-base "$WORK_BASE" --app-db "$WORK_BASE-apps.db" \
  --log-format json --log-level info \
  >"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

cleanup() {
  if kill -0 "$DAEMON_PID" 2>/dev/null; then
    echo "== stopping daemon (pid $DAEMON_PID)"
    kill -TERM "$DAEMON_PID" || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  if [[ -n "${DAEMON_LOG:-}" && -f "$DAEMON_LOG" ]]; then
    echo "----- daemon log tail -----"
    tail -40 "$DAEMON_LOG" || true
    echo "---------------------------"
  fi
}
trap 'cleanup; rm -f "$DAEMON_LOG"' EXIT

echo "== waiting for /healthz"
for _ in {1..50}; do
  if curl -sf "$BASE_URL/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
curl -sf "$BASE_URL/healthz" >/dev/null || { echo "daemon never became healthy" >&2; exit 1; }

# Parse stdout frames: each frame is [1B type][3B reserved][4B big-endian
# size][payload]. type 1 = stdout, 2 = stderr, 3 = exit (JSON payload).
read_stdout() {
  python3 - "$1" <<'PY'
import struct, sys
with open(sys.argv[1], 'rb') as f:
    data = f.read()
off = 0
chunks = []
while off + 8 <= len(data):
    typ = data[off]
    size = struct.unpack('>I', data[off+4:off+8])[0]
    body = data[off+8:off+8+size]
    off += 8 + size
    if typ == 1:
        chunks.append(body.decode(errors='replace'))
sys.stdout.write(''.join(chunks))
PY
}

# exec_in <sandbox-id> <shell-command> — run via the guest agent,
# print the command's stdout.
exec_in() {
  local sb="$1" cmd="$2" out
  out=$(mktemp)
  curl -sSf -X POST "$BASE_URL/sandboxes/$sb/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"cmd\":[\"/bin/sh\",\"-c\",$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$cmd")]}" \
    --output "$out"
  read_stdout "$out"
  rm -f "$out"
}

echo "== creating source sandbox"
SRC=$(curl -sSf -X POST "$BASE_URL/sandboxes" \
        -H 'Content-Type: application/json' \
        -d '{"vcpus":1,"memory_mib":256}' \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "   source id: $SRC"

SRC_MID=$(exec_in "$SRC" 'cat /etc/machine-id' | tr -d '[:space:]')
echo "   source machine-id: $SRC_MID"
if [[ -z "$SRC_MID" ]]; then
  echo "FAIL: could not read source machine-id" >&2
  exit 1
fi

echo "== starting straddling process (sleeps ${STRADDLE_SLEEP}s, then reads /dev/urandom)"
exec_in "$SRC" "setsid sh -c 'sleep $STRADDLE_SLEEP; head -c16 /dev/urandom | od -An -tx1 | tr -d \" \\n\" > /tmp/straddle-rand' </dev/null >/dev/null 2>&1 &" >/dev/null

echo "== creating snapshot (mid-sleep)"
SNAP=$(curl -sSf -X POST "$BASE_URL/sandboxes/$SRC/snapshot" \
       | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "   snapshot id: $SNAP"

echo "== forking count=2"
FORK_RESP=$(mktemp)
FORK_STATUS=$(curl -sS -o "$FORK_RESP" -w '%{http_code}' -X POST "$BASE_URL/snapshots/$SNAP/fork?count=2")
if [[ "$FORK_STATUS" != "201" ]]; then
  echo "   fork failed with HTTP $FORK_STATUS" >&2
  echo "   response body:" >&2
  cat "$FORK_RESP" >&2
  echo >&2
  rm -f "$FORK_RESP"
  exit 1
fi
FORK_IDS=$(python3 -c 'import sys,json;print(" ".join(s["id"] for s in json.load(sys.stdin)["sandboxes"]))' < "$FORK_RESP")
rm -f "$FORK_RESP"
FORK_ARR=($FORK_IDS)
if [[ ${#FORK_ARR[@]} -ne 2 ]]; then
  echo "expected 2 forks, got ${#FORK_ARR[@]}" >&2
  exit 1
fi
FORK_A="${FORK_ARR[0]}"
FORK_B="${FORK_ARR[1]}"
echo "   forks: $FORK_A $FORK_B"

FAILURES=0
fail() { echo "   FAIL: $*" >&2; FAILURES=$((FAILURES + 1)); }
ok()   { echo "   OK   $*"; }

echo "== [1] machine-id rotation"
MID_A=$(exec_in "$FORK_A" 'cat /etc/machine-id' | tr -d '[:space:]')
MID_B=$(exec_in "$FORK_B" 'cat /etc/machine-id' | tr -d '[:space:]')
echo "   fork A: $MID_A"
echo "   fork B: $MID_B"
if [[ -z "$MID_A" || -z "$MID_B" ]]; then
  fail "empty machine-id in a fork"
elif [[ "$MID_A" == "$MID_B" ]]; then
  fail "forks share a machine-id"
elif [[ "$MID_A" == "$SRC_MID" || "$MID_B" == "$SRC_MID" ]]; then
  fail "a fork kept the source's machine-id"
else
  ok "machine-id differs pairwise and from source"
fi

echo "== [2] kernel CRNG divergence (/proc/sys/kernel/random/uuid)"
UUID_A=$(exec_in "$FORK_A" 'cat /proc/sys/kernel/random/uuid' | tr -d '[:space:]')
UUID_B=$(exec_in "$FORK_B" 'cat /proc/sys/kernel/random/uuid' | tr -d '[:space:]')
echo "   fork A: $UUID_A"
echo "   fork B: $UUID_B"
if [[ -z "$UUID_A" || -z "$UUID_B" || "$UUID_A" == "$UUID_B" ]]; then
  fail "kernel uuid identical or unreadable across forks"
else
  ok "uuid differs across forks"
fi

echo "== [3] straddling process divergence (waiting for /tmp/straddle-rand)"
STRADDLE_A=""
STRADDLE_B=""
for _ in {1..45}; do
  STRADDLE_A=$(exec_in "$FORK_A" 'cat /tmp/straddle-rand 2>/dev/null' | tr -d '[:space:]')
  STRADDLE_B=$(exec_in "$FORK_B" 'cat /tmp/straddle-rand 2>/dev/null' | tr -d '[:space:]')
  if [[ -n "$STRADDLE_A" && -n "$STRADDLE_B" ]]; then
    break
  fi
  sleep 1
done
echo "   fork A: ${STRADDLE_A:-<missing>}"
echo "   fork B: ${STRADDLE_B:-<missing>}"
if [[ -z "$STRADDLE_A" || -z "$STRADDLE_B" ]]; then
  fail "straddling process never wrote its bytes (timing? increase STRADDLE_SLEEP)"
elif [[ "$STRADDLE_A" == "$STRADDLE_B" ]]; then
  fail "straddling process read IDENTICAL bytes in both forks — CRNG was not reseeded before the read"
else
  ok "straddling process diverged across forks"
fi

echo "== [4] VMGenID reseed notice (REQUIRED)"
VMGENID_A=$(exec_in "$FORK_A" 'dmesg 2>/dev/null | grep -c "virtual machine fork" || true' | tr -d '[:space:]')
echo "   fork A dmesg matches: ${VMGENID_A:-0}"
if [[ -z "$VMGENID_A" || "$VMGENID_A" == "0" ]]; then
  fail "VMGenID did NOT fire: no 'crng reseeded due to virtual machine fork' in dmesg. The guest kernel likely lacks CONFIG_VMGENID — this is a gap to fix; the agent reseed still ran, but the resume-instant window is unprotected."
else
  ok "kernel logged the vmfork reseed"
fi

echo "== [5] fork identity plumbing (hostname + fork-id marker)"
for F in "$FORK_A" "$FORK_B"; do
  HN=$(exec_in "$F" 'cat /proc/sys/kernel/hostname' | tr -d '[:space:]')
  FID=$(exec_in "$F" 'cat /run/crucible/fork-id' | tr -d '[:space:]')
  if [[ "$HN" != "$F" ]]; then
    fail "$F hostname is '$HN', want the sandbox id"
  elif [[ "$FID" != "$F" ]]; then
    fail "$F fork-id marker is '$FID', want the sandbox id"
  else
    ok "$F hostname + fork-id marker correct"
  fi
done

echo "== tearing down"
for F in $SRC "$FORK_A" "$FORK_B"; do
  curl -sSf -X DELETE "$BASE_URL/sandboxes/$F" -o /dev/null || echo "   (delete $F: already gone?)"
done
curl -sSf -X DELETE "$BASE_URL/snapshots/$SNAP" -o /dev/null || true

if [[ "$FAILURES" -ne 0 ]]; then
  echo "== FAIL — $FAILURES clone-safety check(s) failed"
  exit 1
fi
echo "== PASS — machine-id, CRNG, straddling process, VMGenID, hostname/fork-id all verified"
exit 0
