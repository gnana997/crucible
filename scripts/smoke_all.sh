#!/usr/bin/env bash
#
# Run the full smoke suite in sequence — a pre-release "is everything green?"
# check. Each sub-smoke spins up its own daemon; they default to the same
# chroot-base as a systemd-managed daemon, so STOP that first:
#
#     sudo systemctl stop crucible
#
# Every smoke tests the local ./crucible build (not the installed binary), so
# `make build` before running. Continues past a failure and prints a summary
# at the end; exits non-zero if any smoke failed. Each smoke is bounded by a
# timeout so a wedged one can't block the whole run.
#
# Usage:
#   make build
#   sudo FIRECRACKER_BIN=/usr/local/bin/firecracker \
#        JAILER_BIN=/usr/local/bin/jailer \
#        KERNEL=/var/lib/crucible/vmlinux \
#        ROOTFS=./assets/rootfs-with-agent.ext4 \
#        scripts/smoke_all.sh
#
# Skip the KVM smokes (only run the unprivileged conversion one) with NO_KVM=1.

set -u
set -o pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CRUCIBLE_BIN="${CRUCIBLE_BIN:-./crucible}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/usr/local/bin/jailer}"
KERNEL="${KERNEL:-/var/lib/crucible/vmlinux}"
ROOTFS="${ROOTFS:-./assets/rootfs-with-agent.ext4}"
PER_SMOKE_TIMEOUT="${PER_SMOKE_TIMEOUT:-900}" # seconds

export CRUCIBLE_BIN FIRECRACKER_BIN JAILER_BIN KERNEL ROOTFS

# Each smoke listed with the extra env it needs. "kvm" = root+KVM+FC/jailer;
# "kvm+rootfs" = also needs a profile ROOTFS; "plain" = unprivileged.
#
# Ports are distinct per script, but they share the chroot-base, so they run
# strictly sequentially (never in parallel).
SMOKES=(
  "plain      smoke_image.sh"        # OCI conversion pipeline (no KVM)
  "kvm        smoke_oci.sh"          # boot a converted image (PID-1 agent)
  "kvm        smoke_run_image.sh"    # one-command create --image
  "kvm        smoke_testapp.sh"      # host port publish + curl from host
  "kvm        smoke_shell.sh"        # interactive shell (M-series feature)
  "kvm        smoke_ws_exec.sh"      # WebSocket interactive exec (cross-language transport)
  "kvm        smoke_durable.sh"      # durable apps survive a daemon restart (v0.4)
  "kvm        smoke_egress.sh"       # A6 egress modes + SSRF tripwire (v0.4.1)
  "kvm        smoke_proxy.sh"        # ingress proxy: reach-by-name + isolation (v0.4.2)
  "kvm        smoke_zerodowntime.sh" # zero-downtime rolling app update (v0.4.3)
  "kvm        smoke_cp.sh"           # crucible cp push (files into a sandbox)
  "kvm        smoke_build_run.sh"    # crucible build + run + stop/rm + --rm
  "kvm        smoke_reap.sh"         # orphan VMM + cgroup reaping
  "kvm        smoke_logs.sh"         # durable logs
  "kvm+rootfs smoke_e2e.sh"          # core exec / network / timeout / OOM
  "kvm+rootfs smoke_fork.sh"         # snapshot + fork fan-out
  "kvm+rootfs smoke_clone_safety.sh" # per-fork identity / RNG divergence
  "kvm+rootfs smoke_restart.sh"      # reconcile-on-restart
  "kvm+rootfs smoke_service.sh"      # supervised-service API
  "kvm+rootfs smoke_mcp.sh"          # MCP server end-to-end
)

echo "==============================================================="
echo " crucible full smoke suite"
echo "==============================================================="
"$CRUCIBLE_BIN" version 2>/dev/null || { echo "error: $CRUCIBLE_BIN not runnable (make build)"; exit 2; }
if systemctl is-active --quiet crucible 2>/dev/null; then
  echo "warning: the systemd 'crucible' daemon is ACTIVE — its VMs share the"
  echo "         chroot-base and a smoke's startup reap will kill them."
  echo "         Run:  sudo systemctl stop crucible   then re-run this."
  exit 2
fi
echo "==============================================================="

PASS=0; FAIL=0; SKIP=0
declare -a RESULTS

run_one() {
  local kind="$1" script="$2" path="$HERE/$2"
  if [[ ! -x "$path" ]]; then RESULTS+=("SKIP $script (not found)"); SKIP=$((SKIP+1)); return; fi
  if [[ "$kind" == plain* ]]; then :; elif [[ "${NO_KVM:-0}" == "1" ]]; then
    RESULTS+=("SKIP $script (NO_KVM)"); SKIP=$((SKIP+1)); return
  fi
  echo; echo ">>> $script"
  if timeout "$PER_SMOKE_TIMEOUT" bash "$path"; then
    RESULTS+=("PASS $script"); PASS=$((PASS+1))
  else
    local rc=$?
    RESULTS+=("FAIL $script (rc=$rc)"); FAIL=$((FAIL+1))
  fi
}

for entry in "${SMOKES[@]}"; do
  run_one $entry   # word-split intentional: "kind script"
done

echo; echo "==============================================================="
echo " smoke suite summary"
echo "==============================================================="
for r in "${RESULTS[@]}"; do echo "  $r"; done
echo "---------------------------------------------------------------"
echo "  $PASS passed, $FAIL failed, $SKIP skipped"
echo "==============================================================="
[[ "$FAIL" -eq 0 ]]
