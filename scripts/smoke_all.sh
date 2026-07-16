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
# Reporting: each smoke's output is captured under a log dir (SMOKE_LOGDIR, else
# a mktemp dir). The end-of-run report is TWO tables — a per-file PASS/FAIL/SKIP
# summary with timings, then a "FAILING TEST CASES" table that lists, for every
# failed smoke, the exact failing assertion lines (the ✗ / FAIL: markers) so all
# failures across the whole run are visible in one place. Per-smoke logs are kept.
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
#
# Covers the full feature set — core runtime, apps, networking, scale-to-zero,
# volumes, encryption, backups (incl. incremental), secrets, TLS, observability.
# NOT included (run separately): smoke_installed.sh (tests the installed binary,
# not ./crucible) and smoke_upgrade.sh (needs the previous release tag).

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
  "kvm        smoke_egress.sh"       # egress modes + SSRF tripwire (v0.4.1)
  "kvm        smoke_proxy.sh"        # ingress proxy: reach-by-name + isolation (v0.4.2)
  "kvm        smoke_app_to_app.sh"   # app→app networking: grant/deny + wake + peer isolation (v0.5.1)
  "kvm        smoke_app_scaleout.sh" # horizontal scale-out: autoscale + P2C balance + warm-fork (v0.5.2)
  "kvm        smoke_zerodowntime.sh" # zero-downtime rolling app update (v0.4.3)
  "kvm        smoke_leaks.sh"        # resource + data leaks: drain/rm/scale/sleep orphans + isolation
  "kvm        smoke_observability.sh" # per-app Prometheus metrics off the proxy + daemon pprof (v0.5.4)
  "kvm        smoke_registry.sh"    # private-registry credentials (v0.4.4)
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
  "kvm        smoke_oci_compat.sh"   # OCI compat ten (assorted real images boot)
  # --- scale-to-zero / serverless ---
  "kvm        smoke_app_sleepwake.sh"        # app sleep/wake in place (v0.5.0)
  "kvm        smoke_app_autosleep.sh"        # idle auto-sleep + wake-on-request (v0.5.0)
  "kvm        smoke_sleep_restart.sh"        # a slept app survives a daemon restart (v0.6.4)
  "kvm        smoke_sleepwake_correctness.sh" # wake correctness (clock/RNG/identity)
  "kvm        smoke_serverless.sh"           # wake-on-TCP: serverless postgres/redis (v0.6.1)
  # --- persistence: volumes / encryption / grow ---
  "kvm        smoke_volumes.sh"        # persistent volumes: fsync-honest, single-writer (v0.6.0)
  "kvm        smoke_volume_encrypt.sh" # encryption at rest: LUKS + crypto-shred (v0.8.0)
  "kvm        smoke_key_rotation.sh"   # encryption key management: keyring + rewrap (v0.8.1)
  "kvm        smoke_volume_grow.sh"    # grow a volume in place, plaintext + encrypted (v0.9.1)
  # --- backups ---
  "kvm        smoke_backups.sh"            # volume backups: consistency-by-state + fsfreeze (v0.6.3)
  "kvm        smoke_offhost_backup.sh"     # off-host backup export/import (v0.6.6)
  "kvm        smoke_incremental_backup.sh" # incremental backup chain + restore + off-host (v0.9.3)
  "kvm        smoke_daemon_backup.sh"      # daemon state backup (admin backup) (v0.6.4)
  # --- config / secrets / TLS / lifecycle ops ---
  "kvm        smoke_secrets.sh"       # encrypted secret bundles + envFrom (v0.7.4)
  "kvm        smoke_tls.sh"           # proxy TLS termination + ACME + custom domains (v0.7.0)
  "kvm        smoke_usage.sh"         # persistent per-app usage metrics incl. egress (v0.7.1/0.7.2)
  "kvm        smoke_events.sh"        # app lifecycle event stream (v0.7.3)
  "kvm        smoke_guest_scrape.sh"  # scrape a guest's own /metrics (v0.9.0)
  # --- integration: many features composed on one stateful workload ---
  "kvm        smoke_kitchen_sink.sh"  # encrypted vol + secret + postgres: sleep/wake × backup × grow × restore
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
SUITE_START=$(date +%s)
declare -a RESULTS
declare -a FAILED_SCRIPTS
# Per-smoke output is captured here so the end-of-run report can list the exact
# failing test cases (not just which files failed). Kept for inspection.
LOGDIR="${SMOKE_LOGDIR:-$(mktemp -d 2>/dev/null || echo /tmp/crucible-smoke-logs.$$)}"
mkdir -p "$LOGDIR"

run_one() {
  local kind="$1" script="$2" path="$HERE/$2"
  if [[ ! -x "$path" ]]; then RESULTS+=("SKIP $script (not found)"); SKIP=$((SKIP+1)); return; fi
  if [[ "$kind" == plain* ]]; then :; elif [[ "${NO_KVM:-0}" == "1" ]]; then
    RESULTS+=("SKIP $script (NO_KVM)"); SKIP=$((SKIP+1)); return
  fi
  echo; echo ">>> $script"
  local t0 t1 dt rc log="$LOGDIR/$script.log"
  t0=$(date +%s)
  # tee so output is still shown live AND captured for the failure report;
  # pipefail + PIPESTATUS[0] recover the smoke's real exit code past the tee.
  if timeout "$PER_SMOKE_TIMEOUT" bash "$path" 2>&1 | tee "$log"; then
    t1=$(date +%s); dt=$((t1 - t0))
    RESULTS+=("PASS $script (${dt}s)"); PASS=$((PASS+1))
  else
    rc=${PIPESTATUS[0]}
    t1=$(date +%s); dt=$((t1 - t0))
    # Exit 77 (autotools convention) is a self-skip — e.g. a smoke that needs a
    # capability the current rootfs/agent lacks. Count it as SKIP, not FAIL.
    if [[ "$rc" -eq 77 ]]; then
      RESULTS+=("SKIP $script (${dt}s, self-skip)"); SKIP=$((SKIP+1))
    else
      RESULTS+=("FAIL $script (rc=$rc, ${dt}s)"); FAIL=$((FAIL+1))
      FAILED_SCRIPTS+=("$script")
    fi
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
SUITE_MIN=$(( ($(date +%s) - SUITE_START) / 60 ))
echo "  $PASS passed, $FAIL failed, $SKIP skipped  (${SUITE_MIN} min total)"
echo "==============================================================="

# Consolidated failure table: for each failed smoke, the exact failing test-case
# lines (the sub-smokes mark them with ✗ or FAIL:). So every failure across the
# whole run is visible in one place at the end.
if [[ "$FAIL" -gt 0 ]]; then
  echo
  echo "==============================================================="
  echo " FAILING TEST CASES  (smoke → case)"
  echo "==============================================================="
  for script in "${FAILED_SCRIPTS[@]}"; do
    echo "  ▼ $script"
    cases=$(grep -aE '✗|FAIL' "$LOGDIR/$script.log" 2>/dev/null | grep -avE '[0-9]+ (passed|failed)')
    if [[ -n "$cases" ]]; then
      echo "$cases" | sed 's/^[[:space:]]*/      /'
    else
      echo "      (no per-case marker — smoke exited nonzero/timed out; see the log)"
    fi
    echo "      log: $LOGDIR/$script.log"
  done
  echo "==============================================================="
fi

[[ "$FAIL" -eq 0 ]]
