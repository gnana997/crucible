# Upgrading the daemon without losing your apps

A daemon restart never survives *ephemeral sandboxes* — that's their contract.
Durable **apps** are different: a slept app is a snapshot on disk plus a
record in the app store, and the daemon re-adopts both on startup. That makes
the upgrade story a composition of shipped machinery — drain to sleep, swap
the binary, wake on demand — rather than a feature you wait for:

- brief per-app pause (sub-second wake for most apps), not an outage window
- nothing is lost: identity, volumes, and warm memory state all come back
- the first request to each app wakes it — you don't even have to wake them

## The runbook

```bash
# 1. Seatbelt: capture the control-plane state first (see docs/backups.md).
crucible admin backup

# 2. Drain: sleep every running app.
crucible app sleep --all
crucible app ls          # every app should show asleep

# 3. Swap the binary.
sudo systemctl stop crucible
curl -fsSL https://raw.githubusercontent.com/gnana997/crucible/main/install.sh | sudo bash
#   (or however you install: package, `make build` + copy, …)
sudo systemctl start crucible

# 4. Verify re-adoption: apps are back, still asleep, zero VMs running.
crucible app ls          # asleep — records + snapshots re-adopted
pgrep -c firecracker     # 0

# 5. Done. Apps wake on their first request; warm any eagerly if you like:
crucible app wake db
```

Ephemeral sandboxes (`crucible run`, `sandbox create`) do **not** survive the
restart — the startup reaper cleans up anything the stopped daemon left. If
work in a sandbox matters, snapshot it first (`sandbox snapshot`).

## What a post-upgrade wake actually does

After a daemon restart a wake is not the in-place resume used during normal
scale-to-zero: the new daemon **forks a fresh instance from the durable
snapshot** — restored warm memory, same volumes reattached, clock stepped,
CRNG reseeded. The restored memory still contains the *old release's* guest
agent, so cross-version compatibility of the agent protocol is what makes a
warm wake work across an upgrade.

That compatibility is rehearsed by robots, not assumed:
`scripts/smoke_upgrade.sh` builds the previous release and the current tree,
sleeps a stateless app and a volume-backed app under the old daemon, restarts
onto the new binary, and asserts both wake and serve. Run it before releasing.

**If a warm wake fails after an upgrade** (an incompatible agent change — the
rehearsal is designed to catch this before it ships):

- **Volume apps fall back automatically**: the wake path cold-creates a fresh
  instance from the app's image with the volume reattached. Slower (real boot,
  e.g. WAL recovery), but data-safe and hands-off.
- **Stateless apps**: `crucible app update <name>` (a fresh rollout from the
  image) brings the app back cold; only warm memory state is lost.

Cold boots after an upgrade always run the **new** agent: the converted-image
cache is keyed by the agent digest, so the first cold boot on a new daemon
re-converts the image instead of booting a stale agent.

## Host reboots

The same machinery covers an unclean host restart: slept apps survive by
construction (files + records) and re-adopt on daemon start. Apps that were
*running* at power loss are recreated from their image per their restart
policy. Sleeping the fleet before a planned reboot (`app sleep --all`) turns
it into the upgrade case above.
