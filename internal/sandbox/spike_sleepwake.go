package sandbox

// SPIKE (v0.5.0 items B3/B4) — de-risk the sleep/wake snapshot flow.
//
// This file is a throwaway experiment, NOT the production B3/B4. It proves the
// single riskiest mechanical claim in the scale-to-zero plan: that we can
//
//   snapshot a running guest → kill its VMM (freeing RAM) → keep the netns and
//   the sandbox record → restore in place into the SAME netns/IP/id.
//
// Deliberately OUT of the spike (they belong to the real B1/B3/B4):
//   - no drain / StopSignal grace (B6), no fs quiesce/sync (E6)
//   - no RNG reseed and no guest-clock step on wake (E1/E4) — wake-in-place
//     keeps identity, so "do nothing" is the correct default to observe here
//   - no app model, no `asleep` phase, no reconciler awareness (S1/A2)
//   - sleep state is held in a package map, not on the Sandbox struct, so the
//     whole spike is one deletable file.
//
// What to watch when running scripts/spike_sleepwake.sh on real KVM:
//   1. Does Restore accept reusing the SAME jailer chroot / sandbox id? (fork
//      always allocates a fresh id, so this path is genuinely unexercised.)
//   2. Is the firecracker/jailer process actually gone while asleep (RAM freed)?
//   3. Does the guest's TCP listener survive snapshot→restore? (E2)
//   4. Same guest IP after wake, with the host port-forwarder still working?

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gnana997/crucible/internal/agentapi"
	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/runner"
)

// spikeSleepStates maps a sandbox id → the snapshot artifacts captured at
// SleepInPlace, consumed by WakeInPlace. Package-scoped to keep the spike
// self-contained (no edits to the Sandbox struct).
var spikeSleepStates sync.Map // id string -> *spikeSleepState

type spikeSleepState struct {
	statePath  string
	memPath    string
	rootfsPath string
	netns      string
	vcpus      int
	memMiB     int
}

// SleepInPlace snapshots the guest, then kills its VMM while keeping the netns,
// workdir, and in-memory record — the "snapshot then stop, don't tear down"
// half of C2. The sandbox stays registered; WakeInPlace restores it.
func (m *Manager) SleepInPlace(ctx context.Context, id string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout(s.MemoryMiB))
	defer cancel()

	sleepDir := filepath.Join(s.Workdir, "sleep")
	if err := os.MkdirAll(sleepDir, 0o750); err != nil {
		return fmt.Errorf("spike sleep: mkdir %s: %w", sleepDir, err)
	}
	statePath := filepath.Join(sleepDir, snapshotStateName)
	memPath := filepath.Join(sleepDir, snapshotMemoryName)

	if err := s.handle.Pause(ctx); err != nil {
		return fmt.Errorf("spike sleep: pause %s: %w", id, err)
	}
	if err := s.handle.Snapshot(ctx, statePath, memPath); err != nil {
		// Best-effort: leave the guest running rather than half-slept.
		_ = s.handle.Resume(ctx)
		return fmt.Errorf("spike sleep: snapshot %s: %w", id, err)
	}
	// Kill the VMM to free RAM. Crucially we do NOT call Network.Teardown and
	// do NOT remove the workdir — that identity must survive for the wake.
	if err := s.handle.Shutdown(ctx); err != nil {
		return fmt.Errorf("spike sleep: shutdown vmm %s: %w", id, err)
	}

	st := &spikeSleepState{
		statePath:  statePath,
		memPath:    memPath,
		rootfsPath: filepath.Join(s.Workdir, perSandboxRootfsName),
		vcpus:      s.VCPUs,
		memMiB:     s.MemoryMiB,
	}
	if s.Network != nil {
		st.netns = s.Network.NetnsPath
	}
	spikeSleepStates.Store(id, st)
	return nil
}

// WakeInPlace restores a slept sandbox into its ORIGINAL identity: same id,
// same workdir + rootfs (so writes carry across the cycle), same reserved
// netns. It intentionally does no identity/network/clock refresh — that is the
// wake-vs-fork distinction the real B4 will formalize (E1/E4).
func (m *Manager) WakeInPlace(ctx context.Context, id string) error {
	v, ok := spikeSleepStates.Load(id)
	if !ok {
		return fmt.Errorf("spike wake: sandbox %s is not asleep", id)
	}
	st := v.(*spikeSleepState)

	s, err := m.Get(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout(st.memMiB))
	defer cancel()

	// Stale host-side sockets from the previous boot would collide with the
	// runner recreating them in the reused workdir. Best-effort removal; names
	// that don't exist (e.g. under jailer, where they live in the chroot) are
	// harmless no-ops.
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
	handle, err := m.cfg.Runner.Restore(ctx, spec)
	if err != nil {
		return fmt.Errorf("spike wake: restore %s: %w", id, err)
	}

	// Swap the dead handle for the freshly-restored one. No identity rotation,
	// no re-DHCP, no clock step — that is the whole point of wake-in-place.
	m.mu.Lock()
	s.handle = handle
	s.VSockPath = handle.VSockPath()
	if s.VSockPath != "" {
		s.execClient = agentapi.NewClient(s.VSockPath, agentwire.AgentVSockPort)
	}
	m.mu.Unlock()

	spikeSleepStates.Delete(id)
	return nil
}
