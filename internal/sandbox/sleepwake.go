package sandbox

// In-place sleep/wake — the sandbox-level primitives behind scale-to-zero
// (v0.5.0 items B3/B4). Sleep snapshots a running guest and stops its VMM to
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
	"github.com/gnana997/crucible/internal/runner"
)

// sleepState holds the snapshot artifacts captured when a sandbox is put to
// sleep in place, consumed to wake it.
type sleepState struct {
	statePath  string
	memPath    string
	rootfsPath string
	netns      string
	vcpus      int
	memMiB     int
}

// SleepInPlace snapshots a running sandbox, then stops its VMM to free RAM while
// KEEPING the netns, workdir, and in-memory record — the "snapshot then stop,
// don't tear down" half of sleep (C2). The sandbox stays registered; the /30 is
// still reserved; WakeInPlace restores it into the same identity.
//
// Ordering matters: the guest filesystems are synced (quiesce) while the VM can
// still run — before Pause — so the frozen rootfs copy is clean rather than
// merely crash-consistent. Quiesce is advisory (a journaling fs recovers from a
// crash-consistent image), so a sync failure is logged, never fatal.
func (m *Manager) SleepInPlace(ctx context.Context, id string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout(s.MemoryMiB))
	defer cancel()

	if s.execClient != nil {
		if err := s.execClient.Quiesce(ctx); err != nil {
			slog.Default().Warn("sleep: quiesce failed; snapshot will be crash-consistent",
				"component", "sandbox", "id", id, "err", err)
		}
	}

	sleepDir := filepath.Join(s.Workdir, "sleep")
	if err := os.MkdirAll(sleepDir, 0o750); err != nil {
		return fmt.Errorf("sleep: mkdir %s: %w", sleepDir, err)
	}
	statePath := filepath.Join(sleepDir, snapshotStateName)
	memPath := filepath.Join(sleepDir, snapshotMemoryName)

	if err := s.handle.Pause(ctx); err != nil {
		return fmt.Errorf("sleep: pause %s: %w", id, err)
	}
	if err := s.handle.Snapshot(ctx, statePath, memPath); err != nil {
		// Best-effort: leave the guest running rather than half-slept.
		_ = s.handle.Resume(ctx)
		return fmt.Errorf("sleep: snapshot %s: %w", id, err)
	}
	// Stop the VMM to free RAM. Deliberately NO Network.Teardown and NO workdir
	// removal — that identity must survive for the wake.
	if err := s.handle.Shutdown(ctx); err != nil {
		return fmt.Errorf("sleep: stop vmm %s: %w", id, err)
	}

	st := &sleepState{
		statePath:  statePath,
		memPath:    memPath,
		rootfsPath: filepath.Join(s.Workdir, perSandboxRootfsName),
		vcpus:      s.VCPUs,
		memMiB:     s.MemoryMiB,
	}
	if s.Network != nil {
		st.netns = s.Network.NetnsPath
	}
	m.mu.Lock()
	s.asleep = st
	m.mu.Unlock()
	return nil
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
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout(st.memMiB))
	defer cancel()

	// Stale host-side sockets from the previous boot would collide with the
	// runner recreating them in the reused workdir. Best-effort; missing names
	// (e.g. under jailer, inside the chroot) are harmless no-ops.
	_ = os.Remove(filepath.Join(s.Workdir, "api.sock"))
	_ = os.Remove(filepath.Join(s.Workdir, "vsock.sock"))

	spec := runner.RestoreSpec{
		Workdir:    s.Workdir,
		StatePath:  st.statePath,
		MemPath:    st.memPath,
		RootfsPath: st.rootfsPath,
		LazyMem:    true,
		NetNS:      st.netns,
		Quotas:     m.quotasFor(st.vcpus, st.memMiB),
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
