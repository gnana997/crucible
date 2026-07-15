package sandbox

// In-place sleep/wake — the sandbox-level primitives behind scale-to-zero.
// Sleep snapshots a running guest and stops its VMM to
// free RAM while keeping the netns + record; Wake restores it into the SAME
// identity (id, netns, IP), reseeding the CRNG and stepping the clock but — the
// wake-vs-fork distinction — NOT rotating machine-id/hostname or re-DHCPing.
//
// The mechanics (kill VMM, keep netns, restore in place reusing the same jailer
// id) were validated on real KVM before this landed. The app control plane
// drives these via its instance-controller; a slept app is a steady desired
// state the reconciler leaves alone.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/gnana997/crucible/internal/agentapi"
	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/fsutil"
	"github.com/gnana997/crucible/internal/runner"
)

// sleepState holds what an in-place (same-lifetime) wake needs to restore a
// slept sandbox: the durable snapshot's state+mem files, the LIVE rootfs (so
// writes made before sleep persist across the wake), the reserved netns, and
// sizing. A wake after a daemon restart instead forks from the durable snapshot
// (fresh netns, the snapshot's own frozen rootfs), so it needs none of this.
type sleepState struct {
	statePath  string
	memPath    string
	rootfsPath string
	netns      string
	vcpus      int
	memMiB     int
	volumes    []runner.VolumeAttach // re-staged into the wake chroot
}

// SleepInPlace snapshots a running sandbox, then stops its VMM to free RAM while
// KEEPING the netns, workdir, and in-memory record — the "snapshot then stop,
// don't tear down" half of sleep. The sandbox stays registered; the /30 is
// still reserved; WakeInPlace restores it into the same identity.
//
// Ordering matters: the guest filesystems are synced (quiesce) while the VM can
// still run — before Pause — so the frozen rootfs copy is clean rather than
// merely crash-consistent. Quiesce is advisory (a journaling fs recovers from a
// crash-consistent image), so a sync failure is logged, never fatal.
// It returns the id of the durable snapshot it captured, which the app control
// plane persists so the slept app can be re-adopted (and woken from it) after a
// daemon restart.
func (m *Manager) SleepInPlace(ctx context.Context, id string) (string, error) {
	s, err := m.Get(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout(s.MemoryMiB))
	defer cancel()

	// Disk-admission floor: a sleep writes a full guest-RAM-sized memory file
	// (plus state + a rootfs clone) under WorkBase, so refuse before touching the
	// guest when disk is already low — the app stays running rather than filling
	// the disk with a snapshot. Checked before the transition marker so a refused
	// sleep leaves the instance cleanly routable.
	if err := m.admitSleep(); err != nil {
		return "", err
	}

	// Mark the transition before the guest stops answering: from here until the
	// asleep state is published (or the sleep fails and the guest resumes), the
	// instance must not be routed to — ingress sees ErrAsleep and queues the
	// connection behind the wake instead of dialing a paused guest.
	m.mu.Lock()
	s.sleeping = true
	m.mu.Unlock()
	success := false
	defer func() {
		if !success {
			m.mu.Lock()
			s.sleeping = false
			m.mu.Unlock()
		}
	}()

	if s.execClient != nil {
		if err := s.execClient.Quiesce(ctx); err != nil {
			slog.Default().Warn("sleep: quiesce failed; snapshot will be crash-consistent",
				"component", "sandbox", "id", id, "err", err)
		}
	}

	// Capture a DURABLE snapshot (WorkBase/<snapID>, journaled + rootfs cloned),
	// exactly like Manager.Snapshot — so a slept app survives a daemon restart:
	// the existing snapshot re-adoption resurrects it, and a post-restart wake
	// forks a fresh instance from it. A fresh dir per sleep is also what avoids
	// the jailer EACCES on re-snapshotting a woken VM (the runner writes to a
	// distinct chroot path; the host dir is fresh here too).
	snapID, err := NewSnapshotID()
	if err != nil {
		return "", err
	}
	snapDir := filepath.Join(m.cfg.WorkBase, snapID)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return "", fmt.Errorf("sleep: create snapshot dir: %w", err)
	}
	liveRootfs := filepath.Join(s.Workdir, perSandboxRootfsName)
	snapRootfs := filepath.Join(snapDir, snapshotRootfsName)
	snapState := filepath.Join(snapDir, snapshotStateName)
	snapMem := filepath.Join(snapDir, snapshotMemoryName)

	if err := s.handle.Pause(ctx); err != nil {
		_ = os.RemoveAll(snapDir)
		return "", fmt.Errorf("sleep: pause %s: %w", id, err)
	}
	// fsync each attached volume's backing file so committed data is durable
	// on disk before the VMM stops. Firecracker does not flush drive backing
	// files on snapshot, and cache_type=Writeback may leave writes in the host
	// page cache — so a host crash while asleep could otherwise lose rows. The
	// guest was synced (quiesce) before Pause and is now paused, so nothing is in
	// flight. A fsync failure aborts the sleep (resume, stay running) rather than
	// sleeping with un-durable data. The single-writer volume guard is left HELD:
	// SleepInPlace never Deletes the sandbox, so the slept instance keeps its
	// claim and the volume can't be attached elsewhere while it sleeps.
	if m.cfg.VolumeManager != nil {
		for _, vn := range s.volumeNames {
			if err := m.cfg.VolumeManager.Sync(vn); err != nil {
				_ = s.handle.Resume(ctx)
				_ = os.RemoveAll(snapDir)
				return "", fmt.Errorf("sleep: fsync volume %s for %s: %w", vn, id, err)
			}
		}
	}
	if err := fsutil.Clone(liveRootfs, snapRootfs); err != nil {
		_ = s.handle.Resume(ctx) // stay running rather than half-slept
		_ = os.RemoveAll(snapDir)
		return "", fmt.Errorf("sleep: clone rootfs into snapshot: %w", err)
	}
	if err := s.handle.Snapshot(ctx, snapState, snapMem); err != nil {
		_ = s.handle.Resume(ctx)
		_ = os.RemoveAll(snapDir)
		return "", fmt.Errorf("sleep: snapshot %s: %w", id, err)
	}
	// Stop the VMM to free RAM. Deliberately NO Network.Teardown and NO workdir
	// removal — that identity must survive for an in-lifetime wake-in-place.
	if err := s.handle.Shutdown(ctx); err != nil {
		_ = os.RemoveAll(snapDir)
		return "", fmt.Errorf("sleep: stop vmm %s: %w", id, err)
	}
	// Close each encrypted volume's decrypted device now the VMM (its only holder)
	// is gone, so a slept encrypted volume is ciphertext at rest — not left online
	// on the host for the whole sleep. The single-writer guard stays HELD (s is not
	// Deleted); WakeInPlace re-opens the device. Plaintext volumes have no device.
	if m.cfg.VolumeManager != nil {
		for i, vn := range s.volumeNames {
			if i < len(s.volumes) && s.volumes[i].Encrypted {
				_ = m.cfg.VolumeManager.CloseDevice(vn)
			}
		}
	}

	snap := &Snapshot{
		ID:            snapID,
		SourceID:      s.ID,
		VCPUs:         s.VCPUs,
		MemoryMiB:     s.MemoryMiB,
		Dir:           snapDir,
		StatePath:     snapState,
		MemPath:       snapMem,
		RootfsPath:    snapRootfs,
		StaticNetwork: s.StaticNetwork,
		CreatedAt:     time.Now().UTC(),
	}
	if s.Network != nil && s.Network.Allowlist != nil {
		snap.Network = &NetworkConfig{
			Allowlist:  s.Network.Allowlist,
			FullEgress: s.Network.FullEgress,
			CIDRs:      s.Network.CIDRs,
		}
	}

	st := &sleepState{
		statePath:  snapState,
		memPath:    snapMem,
		rootfsPath: liveRootfs, // in-place wake restores against the LIVE rootfs
		vcpus:      s.VCPUs,
		memMiB:     s.MemoryMiB,
		volumes:    s.volumes, // re-staged into the wake chroot at the same paths
	}
	if s.Network != nil {
		st.netns = s.Network.NetnsPath
	}

	m.mu.Lock()
	m.snapshots[snapID] = snap
	oldSnapID := s.memSnapshotID
	s.memSnapshotID = snapID
	s.asleep = st
	s.sleeping = false // asleep now carries the not-routable state
	m.mu.Unlock()
	success = true

	if m.store != nil {
		if err := m.store.putSnapshot(snapshotRecordOf(snap)); err != nil {
			slog.Default().Warn("persist sleep snapshot record failed", "component", "sandbox", "id", snapID, "err", err)
		}
	}

	// Keep exactly one snapshot per instance: the previous one's memory file
	// backed the VM we just shut down, so its uffd pager is now dead and the
	// snapshot is safe to drop.
	if oldSnapID != "" && oldSnapID != snapID {
		_ = m.DeleteSnapshot(context.Background(), oldSnapID)
	}
	return snapID, nil
}

// WakeInPlace restores a slept sandbox into its ORIGINAL identity: same id,
// same workdir + rootfs (so writes carry across the cycle), same reserved
// netns/IP. It reseeds the guest CRNG and steps its clock via the agent /wake
// endpoint — but, unlike a fork, does NOT rotate machine-id/hostname or
// re-DHCP. The reseed+clock are applied to the restored guest BEFORE its handle
// is swapped in, mirroring fork's fatal-before-reachable discipline: replayed
// entropy and a stale clock don't self-heal, so a refresh failure fails the
// wake and stops the freshly-restored VMM.
func (m *Manager) WakeInPlace(ctx context.Context, id string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	m.mu.RLock()
	st := s.asleep
	m.mu.RUnlock()
	if st == nil {
		return fmt.Errorf("wake: sandbox %s is not asleep", id)
	}
	if err := m.admitWake(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout(st.memMiB))
	defer cancel()

	// Stale host-side sockets from the previous boot would collide with the
	// runner recreating them in the reused workdir. Best-effort; missing names
	// (e.g. under jailer, inside the chroot) are harmless no-ops.
	_ = os.Remove(filepath.Join(s.Workdir, "api.sock"))
	_ = os.Remove(filepath.Join(s.Workdir, "vsock.sock"))

	// Re-open each encrypted volume's device, closed at sleep so it was ciphertext
	// at rest, before the runner re-stages its node into the wake chroot. The
	// mapper name is stable, so the paths already in st.volumes stay valid.
	if m.cfg.VolumeManager != nil {
		for i, vn := range s.volumeNames {
			if i < len(st.volumes) && st.volumes[i].Encrypted {
				if _, oerr := m.cfg.VolumeManager.OpenDevice(vn); oerr != nil {
					return fmt.Errorf("wake: reopen encrypted volume %s for %s: %w", vn, id, oerr)
				}
			}
		}
	}

	spec := runner.RestoreSpec{
		Workdir:    s.Workdir,
		StatePath:  st.statePath,
		MemPath:    st.memPath,
		RootfsPath: st.rootfsPath,
		LazyMem:    true,
		NetNS:      st.netns,
		Quotas:     m.quotasFor(st.vcpus, st.memMiB),
		Volumes:    st.volumes, // re-attach the volume drive(s) on wake
	}
	restoreStart := time.Now()
	handle, err := m.cfg.Runner.Restore(ctx, spec)
	if err != nil {
		return fmt.Errorf("wake: restore %s: %w", id, err)
	}
	m.cfg.Metrics.ObserveSnapshotRestore(time.Since(restoreStart))

	// The restored VM already has the agent running inside (its vsock listener
	// survived the snapshot), so reseed+clock is one round-trip on the new
	// channel. A restore with no agent channel can't be refreshed and must fail
	// rather than come up with replayed entropy.
	vsockPath := handle.VSockPath()
	if vsockPath == "" {
		_ = handle.Shutdown(context.Background())
		return fmt.Errorf("wake %s: no agent channel for reseed/clock refresh", id)
	}
	c := agentapi.NewClient(vsockPath, agentwire.AgentVSockPort)
	if err := m.wakeRefresh(ctx, c); err != nil {
		_ = handle.Shutdown(context.Background())
		return fmt.Errorf("wake %s: refresh: %w", id, err)
	}

	m.mu.Lock()
	s.handle = handle
	s.VSockPath = vsockPath
	s.execClient = c
	s.asleep = nil
	// KEEP s.memSnapshotID: the woken VM restored with LazyMem, so its uffd pager
	// serves from that snapshot's memory file for the VM's whole life. It's GC'd
	// on the next sleep (once this VMM, and its pager, are stopped) or by Delete.
	m.mu.Unlock()
	return nil
}

// wakeRefresh reseeds the guest CRNG and steps its clock over the agent /wake
// endpoint, retrying until the agent answers or ctx expires — a just-restored
// agent may need a moment, mirroring refreshIdentity's retry loop. An
// unsupported endpoint (stale rootfs) is returned immediately, not retried.
func (m *Manager) wakeRefresh(ctx context.Context, c *agentapi.Client) error {
	seed := make([]byte, identitySeedSize)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("generate seed: %w", err)
	}
	refreshCtx, cancel := context.WithTimeout(ctx, identityRefreshTimeout)
	defer cancel()

	var lastErr error
	for {
		lastErr = c.Wake(refreshCtx, seed, time.Now().UnixNano())
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, agentapi.ErrWakeUnsupported) {
			return lastErr
		}
		select {
		case <-refreshCtx.Done():
			return fmt.Errorf("%w (last attempt: %v)", refreshCtx.Err(), lastErr)
		case <-time.After(identityRefreshPollInterval):
		}
	}
}
