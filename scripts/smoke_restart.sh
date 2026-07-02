#!/usr/bin/env bash
#
# End-to-end smoke test for crash-safe registries + startup reconcile
# (docs/GAPS.md gap 3).
#
# What this proves:
#   1. A sandbox boots under jailer and a snapshot is taken.
#   2. The daemon is HARD-KILLED (SIGKILL) — no clean shutdown, so the
#      VM process, its workdir, and (with network) its netns/nft state
#      are left behind exactly as a real crash would leave them.
#   3. We confirm those orphans actually exist post-crash.
#   4. The daemon is restarted. Its startup path — jailer.ReapOrphans +
#      network.ReapOrphans + Manager.Reconcile — must:
#        a. leave NO orphaned firecracker processes,
#        b. leave NO orphaned crucible-* netns,
#        c. leave NO per-sandbox nft state for the dead sandbox,
#        d. leave NO leftover sandbox workdir under --work-base,
#        e. RE-ADOPT the snapshot (it survives the restart and is still
#           forkable — the durable-authority payoff).
#
# Requires root because jailer needs CAP_SYS_ADMIN + privilege drop.
# Networking checks (b, c) run only when --network-egress-iface is
# provided via NET_EGRESS_IFACE; without it they are skipped with a note.
#
# Usage:
#   sudo CRUCIBLE_BIN=./crucible \
#        FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        ROOTFS=./assets/rootfs-with-agent.ext4 \
#        [NET_EGRESS_IFACE=eth0] \
#        scripts/smoke_restart.sh
#
# Exits 0 on success, non-zero on any failure.

set -euo pipefail

CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-./assets/rootfs-with-agent.ext4}"
CHROOT_BASE="${CHROOT_BASE:-/srv/jailer}"
WORK_BASE="${WORK_BASE:-/var/lib/crucible/run}"
LISTEN="${LISTEN:-127.0.0.1:7880}"
BASE_URL="http://${LISTEN}"
NET_EGRESS_IFACE="${NET_EGRESS_IFACE:-}"

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (jailer requires CAP_SYS_ADMIN)" >&2
  exit 1
fi
for bin in "$CRUCIBLE_BIN" "$FIRECRACKER_BIN" "$JAILER_BIN"; do
  [[ -x "$bin" ]] || { echo "error: missing or non-executable: $bin" >&2; exit 1; }
done
for f in "$KERNEL" "$ROOTFS"; do
  [[ -r "$f" ]] || { echo "error: missing or unreadable: $f" >&2; exit 1; }
done

DAEMON_LOG=$(mktemp)
DAEMON_PID=""

start_daemon() {
  local net_args=()
  if [[ -n "$NET_EGRESS_IFACE" ]]; then
    net_args=(--network-egress-iface "$NET_EGRESS_IFACE")
  fi
  "$CRUCIBLE_BIN" daemon \
    --listen "$LISTEN" \
    --firecracker-bin "$FIRECRACKER_BIN" \
    --jailer-bin "$JAILER_BIN" \
    --chroot-base "$CHROOT_BASE" \
    --kernel "$KERNEL" \
    --rootfs "$ROOTFS" \
    --work-base "$WORK_BASE" \
    "${net_args[@]}" \
    --log-format json --log-level info \
    >>"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!

  for _ in {1..50}; do
    curl -sf "$BASE_URL/healthz" >/dev/null 2>&1 && return 0
    # bail early if the daemon died on startup
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "daemon exited during startup" >&2; tail -20 "$DAEMON_LOG" >&2; return 1; }
    sleep 0.2
  done
  echo "daemon never became healthy" >&2
  tail -20 "$DAEMON_LOG" >&2
  return 1
}

cleanup() {
  if [[ -n "$DAEMON_PID" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill -TERM "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  echo "----- daemon log tail -----"
  tail -30 "$DAEMON_LOG" 2>/dev/null || true
  echo "---------------------------"
  rm -f "$DAEMON_LOG"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# vm_pids prints the host PIDs of THIS sandbox's jailer + firecracker,
# matched by the jailer ID token in /proc/<pid>/cmdline. We match cmdline
# (not /proc/<pid>/root) because jailer pivot_root's firecracker into a
# private mount namespace, which hides its root from the host — the same
# reason the daemon's own reap matches on cmdline. This keeps the test
# correct on a busy host with other unrelated VMs alive.
vm_pids() {
  local id="$1" p pid
  for p in /proc/[0-9]*; do
    pid=${p#/proc/}
    # Group-level 2>/dev/null so a process exiting mid-scan (the input
    # redirect failing) doesn't spew "No such file or directory".
    if { tr '\0' '\n' < "$p/cmdline"; } 2>/dev/null | grep -Fxq -- "$id"; then
      echo "$pid"
    fi
  done
}

# --- incarnation 1: create sandbox + snapshot -------------------------
echo "== [1] starting daemon (net=${NET_EGRESS_IFACE:-off})"
start_daemon || exit 1

echo "== creating sandbox"
if [[ -n "$NET_EGRESS_IFACE" ]]; then
  BODY='{"vcpus":1,"memory_mib":256,"network":{"enabled":true,"allowlist":["*.example.com"]}}'
else
  BODY='{"vcpus":1,"memory_mib":256}'
fi
SRC=$(curl -sSf -X POST "$BASE_URL/sandboxes" -H 'Content-Type: application/json' -d "$BODY" \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "   sandbox id: $SRC"

echo "== creating snapshot"
SNAP=$(curl -sSf -X POST "$BASE_URL/sandboxes/$SRC/snapshot" \
       | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "   snapshot id: $SNAP"

# Capture orphan-to-be state — scoped to THIS sandbox's jailer ID, so the
# test is immune to any unrelated firecracker VMs already on the host.
SRC_WORKDIR="$WORK_BASE/$SRC"
SRC_ID_SAN=$(printf '%s' "$SRC" | tr '_' '-')
SRC_CHROOT="$CHROOT_BASE/firecracker/$SRC_ID_SAN/root"
mapfile -t VM_PIDS < <(vm_pids "$SRC_ID_SAN")
echo "   this sandbox's VM pids: ${VM_PIDS[*]:-none} (id $SRC_ID_SAN)"
[[ ${#VM_PIDS[@]} -gt 0 ]] || fail "could not find this sandbox's firecracker process before crash"
[[ -d "$SRC_WORKDIR" ]] || fail "expected sandbox workdir $SRC_WORKDIR to exist before crash"

# --- simulate crash: SIGKILL, no clean shutdown -----------------------
echo "== [2] SIGKILL daemon (pid $DAEMON_PID) — simulating crash"
kill -9 "$DAEMON_PID" 2>/dev/null || true
wait "$DAEMON_PID" 2>/dev/null || true
sleep 1

# Confirm the crash actually left orphans (otherwise the test proves nothing).
ALIVE_BEFORE=0
for pid in "${VM_PIDS[@]}"; do
  kill -0 "$pid" 2>/dev/null && ALIVE_BEFORE=$((ALIVE_BEFORE+1))
done
echo "   this sandbox's VM still alive post-crash: $ALIVE_BEFORE/${#VM_PIDS[@]}"
[[ "$ALIVE_BEFORE" -gt 0 ]] || fail "sandbox VM died with the daemon (nothing to reap — test would be vacuous)"
[[ -d "$SRC_WORKDIR" ]] || fail "workdir vanished on crash (unexpected)"
if [[ -n "$NET_EGRESS_IFACE" ]]; then
  NETNS_BEFORE=$(ip netns list 2>/dev/null | grep -c crucible || true)
  echo "   orphaned crucible netns post-crash: $NETNS_BEFORE"
fi

# --- incarnation 2: restart → reconcile -------------------------------
echo "== [3] restarting daemon — reconcile should reap orphans"
DAEMON_PID=""
start_daemon || exit 1
sleep 1

# (a) this sandbox's VM processes were killed by the reap.
STILL_ALIVE=0
for pid in "${VM_PIDS[@]}"; do
  if kill -0 "$pid" 2>/dev/null; then
    echo "   still alive: pid $pid (comm=$(cat /proc/$pid/comm 2>/dev/null))" >&2
    STILL_ALIVE=$((STILL_ALIVE+1))
  fi
done
[[ "$STILL_ALIVE" -eq 0 ]] || fail "$STILL_ALIVE of this sandbox's VM processes survived the restart"
# Belt-and-suspenders: no process still carries this VM's ID.
LINGER=$(vm_pids "$SRC_ID_SAN" | wc -l)
[[ "$LINGER" -eq 0 ]] || fail "$LINGER process(es) still carrying id $SRC_ID_SAN after restart"
echo "   OK: this sandbox's orphaned VM was reaped"

# (d) no leftover sandbox workdir.
[[ -e "$SRC_WORKDIR" ]] && fail "orphaned sandbox workdir $SRC_WORKDIR not reaped"
echo "   OK: sandbox workdir reaped"

# (b,c) network orphans — only when networking was exercised.
if [[ -n "$NET_EGRESS_IFACE" ]]; then
  NETNS_AFTER=$(ip netns list 2>/dev/null | grep -c crucible || true)
  [[ "$NETNS_AFTER" -eq 0 ]] || fail "orphaned crucible netns after restart: $(ip netns list | grep crucible)"
  echo "   OK: no orphaned crucible netns"
  # No per-sandbox nft state referencing the dead sandbox id.
  if nft list table inet crucible >/tmp/nft.$$ 2>/dev/null; then
    if grep -qa "$SRC" /tmp/nft.$$; then
      rm -f /tmp/nft.$$
      fail "nft table still references dead sandbox $SRC"
    fi
    rm -f /tmp/nft.$$
  fi
  echo "   OK: no per-sandbox nft state for dead sandbox"
else
  echo "   (skipping netns/nft checks — set NET_EGRESS_IFACE to exercise them)"
fi

# (e) snapshot survived the restart and is still forkable.
echo "== [4] verifying snapshot re-adoption"
curl -sSf "$BASE_URL/snapshots" | python3 -c "
import sys,json
snaps=json.load(sys.stdin).get('snapshots',[])
ids=[s['id'] for s in snaps]
sys.exit(0 if '$SNAP' in ids else 1)
" || fail "snapshot $SNAP not re-adopted after restart"
echo "   OK: snapshot $SNAP present after restart"

FORK_STATUS=$(curl -sS -o /tmp/fork.$$ -w '%{http_code}' -X POST "$BASE_URL/snapshots/$SNAP/fork?count=1")
if [[ "$FORK_STATUS" != "201" ]]; then
  echo "   fork response:" >&2; cat /tmp/fork.$$ >&2; rm -f /tmp/fork.$$
  fail "fork from re-adopted snapshot returned HTTP $FORK_STATUS"
fi
FORK_ID=$(python3 -c 'import sys,json;print(json.load(sys.stdin)["sandboxes"][0]["id"])' < /tmp/fork.$$)
rm -f /tmp/fork.$$
echo "   OK: forked $FORK_ID from re-adopted snapshot"

# --- teardown ---------------------------------------------------------
echo "== tearing down"
curl -sSf -X DELETE "$BASE_URL/sandboxes/$FORK_ID" -o /dev/null || true
curl -sSf -X DELETE "$BASE_URL/snapshots/$SNAP" -o /dev/null || true

echo "== PASS — crash left orphans, restart reaped processes/netns/nft/workdir, snapshot re-adopted"
exit 0
