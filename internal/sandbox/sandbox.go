// Package sandbox owns the lifecycle of crucible's logical sandboxes.
//
// A Sandbox is the user-facing handle to one running Firecracker VM. The
// Manager maps IDs to Sandboxes, hands out new IDs, and orchestrates the
// three phases of a sandbox's life:
//
//  1. Create — allocate an ID, derive a workdir, call Runner.Start, store
//     the resulting Handle in the map.
//  2. Lookup — Get and List read from the map with an RLock.
//  3. Delete — remove from the map first (so concurrent Gets don't see a
//     half-destroyed sandbox), shut the Handle down, then remove the
//     workdir from disk.
//
// The package is intentionally unaware of HTTP. The daemon package wraps
// Manager in handlers; tests substitute a stub Runner.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/agentapi"
	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/fsutil"
	"github.com/gnana997/crucible/internal/runner"
)

// perSandboxRootfsName is the filename Manager.Create uses for the
// per-sandbox rootfs clone under the workdir. Kept unexported; the
// exact name isn't part of the external contract.
const perSandboxRootfsName = "rootfs.ext4"

// Filenames inside a snapshot directory. Stable so operators can
// inspect and copy snapshot artifacts manually.
const (
	snapshotStateName  = "state.file"
	snapshotMemoryName = "memory.file"
	snapshotRootfsName = "rootfs.ext4"
	perForkMemoryName  = "memory.file"
)

// ErrSnapshotNotFound is returned by Manager.{Get,Delete}Snapshot when
// no snapshot matches the given ID.
var ErrSnapshotNotFound = errors.New("snapshot: not found")

// Default machine sizing applied when a CreateConfig leaves a field at 0.
// These mirror the "sane defaults" principle from docs/VISION.md — small
// enough to be cheap, big enough to run a Python interpreter.
const (
	DefaultVCPUs     = 1
	DefaultMemoryMiB = 512
)

// DefaultAgentReadyTimeout is the time Create will wait for the guest
// agent's /healthz to start answering when WaitForAgent is enabled.
// Bounded higher than a typical microVM boot (~2s) with slack for
// systemd bring-up.
const DefaultAgentReadyTimeout = 15 * time.Second

// agentReadyPollInterval is how often Create re-polls /healthz between
// failed attempts.
const agentReadyPollInterval = 200 * time.Millisecond

// ErrNotFound is returned by Get and Delete when no sandbox has the given
// ID. Callers can errors.Is it.
var ErrNotFound = errors.New("sandbox: not found")

// Sandbox is the in-memory record for one running VM.
//
// Fields are read-only after creation from the Manager's perspective.
// Exposing them as a struct (not behind getters) is a deliberate choice
// to keep the package easy to read; the daemon serializes these directly.
type Sandbox struct {
	ID        string
	VCPUs     int
	MemoryMiB int
	Workdir   string
	VSockPath string // host UDS for Firecracker's hybrid vsock; empty for test stubs
	CreatedAt time.Time

	// Network is nil when the sandbox has no NIC attached (default).
	// Non-nil carries everything the daemon needs to teardown and
	// to echo back in sandboxResponse.
	Network *NetworkHandle

	handle     runner.Handle
	execClient *agentapi.Client // cached; nil when VSockPath is empty

	// done is closed by Manager.Delete once this sandbox is removed
	// from the map. Used by the lifetime-timeout goroutine to exit
	// cleanly when the sandbox is deleted by other means. Callers
	// must not close this themselves.
	done     chan struct{}
	doneOnce sync.Once
}

// CreateConfig is the input to Manager.Create. Zero-valued fields are
// filled in with package defaults.
type CreateConfig struct {
	VCPUs     int
	MemoryMiB int
	BootArgs  string // empty means use runner.DefaultBootArgs

	// TimeoutSec, if > 0, is the sandbox's maximum lifetime in seconds.
	// A background goroutine deletes the sandbox once the timeout fires.
	// Zero disables the timeout (the sandbox lives until an explicit
	// Delete or daemon shutdown).
	TimeoutSec int

	// Network, when non-nil, provisions the sandbox with its own
	// network namespace + per-sandbox DHCP / DNS / firewall
	// policy. Absent (nil) means no NIC at all — default-deny.
	//
	// Enabling this requires the daemon to have a configured
	// ManagerConfig.Network; otherwise Create returns an error
	// rather than silently falling back to no-network.
	Network *NetworkConfig
}

// NetworkConfig declares the per-sandbox network intent. Exactly
// one of Allowlist / nil is meaningful; an empty slice is valid
// (explicit deny of everything except the implicit DNS path).
type NetworkConfig struct {
	// Allowlist must be a validated network.Allowlist; the
	// daemon layer parses the user-supplied patterns and hands
	// the typed value through. Required when NetworkConfig is
	// non-nil.
	Allowlist NetworkAllowlist
}

// NetworkAllowlist decouples sandbox.Manager from internal/network's
// Allowlist type, which keeps the packages' import graph clean.
// In production, the daemon passes *network.Allowlist (which
// satisfies both Matches and Patterns); in tests we use a small
// stub.
type NetworkAllowlist interface {
	Matches(name string) bool
	Patterns() []string
}

// ManagerConfig wires a Manager to its dependencies and defaults. The
// Runner can be a *runner.Firecracker in production or a stub in tests.
type ManagerConfig struct {
	Runner runner.Runner

	// WorkBase is the parent directory under which each sandbox gets its
	// own workdir (WorkBase/<id>/). The Manager creates WorkBase lazily.
	WorkBase string

	// Kernel and Rootfs are applied to every sandbox in v0.1. Profiles
	// and per-sandbox overrides arrive in v0.2.
	//
	// Rootfs is a *template* — Manager.Create clones it to a per-
	// sandbox file under the sandbox workdir before handing the clone
	// to the runner. Sharing one writable rootfs across concurrent VMs
	// would corrupt the filesystem, so the clone step is not optional.
	Kernel string
	Rootfs string

	// WaitForAgent, when true, makes Create block until the guest agent
	// inside the VM responds on GET /healthz (via the vsock UDS). This
	// is the right default for production use with a crucible-agent
	// baked into the rootfs. Leave false when the rootfs doesn't have
	// the agent (dev setups, Checkpoint-Zero-style tests, unit tests).
	WaitForAgent bool

	// AgentReadyTimeout bounds the readiness poll when WaitForAgent is
	// true. Zero means DefaultAgentReadyTimeout.
	AgentReadyTimeout time.Duration

	// Network, when non-nil, enables per-sandbox networking. Only
	// sandboxes whose CreateConfig.Network is non-nil actually use
	// it; when the field itself is nil here, any request with
	// Network set is rejected at Create time.
	Network NetworkProvisioner
}

// NetworkProvisioner is the narrow slice of internal/network.Manager
// that sandbox.Manager depends on. Stated as an interface so the
// two packages stay unordered in the import graph and so tests can
// substitute a trivial stub.
type NetworkProvisioner interface {
	Setup(ctx context.Context, req NetworkSetupRequest) (*NetworkHandle, error)
	Teardown(ctx context.Context, h *NetworkHandle) error
}

// NetworkSetupRequest is the argument to NetworkProvisioner.Setup.
// Mirrors internal/network.SandboxSetup but lives here so
// sandbox.Manager doesn't import the network package directly.
type NetworkSetupRequest struct {
	SandboxID string
	Allowlist NetworkAllowlist
}

// NetworkHandle is the return value of NetworkProvisioner.Setup,
// kept in the Sandbox record so Teardown can be called on Delete.
// The production value is a wrapper constructed by the daemon
// around *network.SandboxHandle; fields here capture what
// sandbox.Manager reads back from it.
type NetworkHandle struct {
	// NetnsPath is the host path of the namespace Firecracker
	// should join (plumbed into runner.Spec.NetNS).
	NetnsPath string

	// TapName is the host device Firecracker attaches to.
	TapName string

	// GuestMAC formatted as "02:ab:cd:ef:01:23".
	GuestMAC string

	// GuestIP is the IP DHCP will hand to the guest. Exposed so
	// the daemon can echo it back to callers in sandboxResponse.
	GuestIP string

	// Gateway is the host-side veth IP (guest's default route).
	Gateway string

	// Allowlist is the matcher used at setup time. Retained on
	// the handle because snapshots copy it onto Snapshot.Network
	// so forks can re-register an identical policy without the
	// daemon having to re-parse patterns. Matchers are
	// immutable; sharing a single instance across source + forks
	// is safe.
	Allowlist NetworkAllowlist

	// Impl is an opaque reference to the network package's own
	// handle type, carried along so Teardown can pass the same
	// pointer back. sandbox.Manager never dereferences it.
	Impl any
}

// Snapshot is a frozen reference point captured from a running sandbox.
// Forks derive from a Snapshot: each fork's initial memory and rootfs
// are cloned from the snapshot directory.
type Snapshot struct {
	ID         string
	SourceID   string // sandbox the snapshot was taken from
	VCPUs      int    // inherited from source
	MemoryMiB  int    // inherited from source
	Dir        string // snapshot directory (StatePath/MemPath/RootfsPath live inside)
	StatePath  string // state.file
	MemPath    string // memory.file
	RootfsPath string // rootfs.ext4 (frozen clone at snapshot time)
	CreatedAt  time.Time

	// Network captures the source sandbox's network intent so
	// forks can be provisioned with matching network state.
	// Nil means the source had no network, and forks get none
	// either.
	Network *NetworkConfig
}

// Manager owns the sandbox + snapshot registries and coordinates
// lifecycle operations.
type Manager struct {
	cfg ManagerConfig

	mu        sync.RWMutex
	sandboxes map[string]*Sandbox
	snapshots map[string]*Snapshot
}

// NewManager constructs a Manager. It does not touch the filesystem or
// start any goroutines; those happen lazily on Create.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.Runner == nil {
		return nil, errors.New("sandbox: ManagerConfig.Runner is required")
	}
	if cfg.WorkBase == "" {
		return nil, errors.New("sandbox: ManagerConfig.WorkBase is required")
	}
	if cfg.Kernel == "" {
		return nil, errors.New("sandbox: ManagerConfig.Kernel is required")
	}
	if cfg.Rootfs == "" {
		return nil, errors.New("sandbox: ManagerConfig.Rootfs is required")
	}
	return &Manager{
		cfg:       cfg,
		sandboxes: make(map[string]*Sandbox),
		snapshots: make(map[string]*Snapshot),
	}, nil
}

// Create allocates a new sandbox, boots its VM, and stores the record.
// If anything fails before the sandbox is registered, Create rolls back
// — the firecracker process (if spawned) is shut down, and the workdir
// and its per-sandbox rootfs clone are removed. Callers can safely retry.
func (m *Manager) Create(ctx context.Context, req CreateConfig) (*Sandbox, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	vcpus := req.VCPUs
	if vcpus <= 0 {
		vcpus = DefaultVCPUs
	}
	memMiB := req.MemoryMiB
	if memMiB <= 0 {
		memMiB = DefaultMemoryMiB
	}
	workdir := filepath.Join(m.cfg.WorkBase, id)

	// success is flipped at the end; if anything below returns early,
	// the deferred cleanup removes the workdir we created.
	var success bool
	defer func() {
		if !success {
			_ = os.RemoveAll(workdir)
		}
	}()

	if err := os.MkdirAll(workdir, 0o750); err != nil {
		return nil, fmt.Errorf("sandbox: create workdir %s: %w", workdir, err)
	}

	// Clone the rootfs template into a per-sandbox copy so concurrent
	// sandboxes don't corrupt each other's filesystem state. On reflink-
	// capable filesystems this is effectively instant; otherwise it's a
	// full byte copy.
	sbxRootfs := filepath.Join(workdir, perSandboxRootfsName)
	if err := fsutil.Clone(m.cfg.Rootfs, sbxRootfs); err != nil {
		return nil, fmt.Errorf("sandbox: clone rootfs template: %w", err)
	}

	// Network setup (optional). Must happen before runner.Start
	// because the runner needs the netns path + tap name for
	// Firecracker's PUT /network-interfaces.
	netHandle, err := m.provisionNetwork(ctx, id, req.Network)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success && netHandle != nil {
			_ = m.cfg.Network.Teardown(context.Background(), netHandle)
		}
	}()

	runnerSpec := runner.Spec{
		Workdir:   workdir,
		Kernel:    m.cfg.Kernel,
		Rootfs:    sbxRootfs,
		BootArgs:  req.BootArgs,
		VCPUs:     vcpus,
		MemoryMiB: memMiB,
	}
	if netHandle != nil {
		runnerSpec.NetNS = netHandle.NetnsPath
		runnerSpec.Net = &runner.NetConfig{
			IfaceID:  "eth0",
			HostDev:  netHandle.TapName,
			GuestMAC: netHandle.GuestMAC,
		}
	}

	handle, err := m.cfg.Runner.Start(ctx, runnerSpec)
	if err != nil {
		return nil, fmt.Errorf("sandbox: start %s: %w", id, err)
	}

	s := &Sandbox{
		ID:        id,
		VCPUs:     vcpus,
		MemoryMiB: memMiB,
		Workdir:   workdir,
		VSockPath: handle.VSockPath(),
		CreatedAt: time.Now().UTC(),
		Network:   netHandle,
		handle:    handle,
		done:      make(chan struct{}),
	}
	if s.VSockPath != "" {
		s.execClient = agentapi.NewClient(s.VSockPath, agentwire.AgentVSockPort)
	}

	if m.cfg.WaitForAgent && s.execClient != nil {
		if err := m.waitForAgent(ctx, s.execClient); err != nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("sandbox: agent not ready: %w", err)
		}
	}

	m.mu.Lock()
	m.sandboxes[id] = s
	m.mu.Unlock()

	if req.TimeoutSec > 0 {
		m.startLifetimeTimer(s, req.TimeoutSec)
	}

	success = true
	return s, nil
}

// startLifetimeTimer deletes s after sec seconds unless s is deleted
// by some other path first. The goroutine exits as soon as s.done is
// closed (which Delete does before shutting the handle down), so
// there's no leak on early deletes.
func (m *Manager) startLifetimeTimer(s *Sandbox, sec int) {
	go func() {
		timer := time.NewTimer(time.Duration(sec) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			// Give the shutdown a comfortable deadline but don't
			// inherit the caller's Create context (it's long gone
			// by the time this fires).
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = m.Delete(ctx, s.ID)
		case <-s.done:
		}
	}()
}

// provisionNetwork is the single place Create and Fork hand off to
// the network provisioner. Returns:
//
//   - (nil, nil) when req is nil — the sandbox wants no network,
//     which is the default-deny case.
//   - (nil, error) when req is non-nil but the daemon lacks a
//     configured NetworkProvisioner. Explicit failure is better
//     than silently attaching nothing and letting the guest boot
//     without the network the caller asked for.
//   - (handle, nil) on success — caller must Teardown on
//     rollback + on Delete.
func (m *Manager) provisionNetwork(ctx context.Context, sandboxID string, req *NetworkConfig) (*NetworkHandle, error) {
	if req == nil {
		return nil, nil
	}
	if m.cfg.Network == nil {
		return nil, errors.New("sandbox: network requested but daemon has no network provisioner configured")
	}
	if req.Allowlist == nil {
		return nil, errors.New("sandbox: NetworkConfig.Allowlist required")
	}
	h, err := m.cfg.Network.Setup(ctx, NetworkSetupRequest{
		SandboxID: sanitizeNetworkID(sandboxID),
		Allowlist: req.Allowlist,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: network setup: %w", err)
	}
	return h, nil
}

// sanitizeNetworkID mirrors runner/jailer's sanitizeJailerID:
// underscores → hyphens, so "sbx_abc" → "sbx-abc". The network
// layer uses this ID in interface names + netns names + nft chain
// names, all of which reject underscores.
func sanitizeNetworkID(id string) string {
	out := make([]byte, len(id))
	for i := 0; i < len(id); i++ {
		if id[i] == '_' {
			out[i] = '-'
			continue
		}
		out[i] = id[i]
	}
	return string(out)
}

// waitForAgent polls /healthz on the guest agent until it responds or
// the readiness deadline elapses. Errors before the deadline are
// treated as "not ready yet" — the agent typically becomes reachable
// only after systemd has brought up its service unit, which takes a
// couple of seconds on top of the VM boot.
func (m *Manager) waitForAgent(ctx context.Context, c *agentapi.Client) error {
	deadline := m.cfg.AgentReadyTimeout
	if deadline <= 0 {
		deadline = DefaultAgentReadyTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var lastErr error
	for {
		if err := c.GetHealthz(readyCtx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-readyCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last poll: %v)", readyCtx.Err(), lastErr)
			}
			return readyCtx.Err()
		case <-time.After(agentReadyPollInterval):
		}
	}
}

// Exec runs a command inside the sandbox via its guest agent and
// streams stdout/stderr to the given writers. The final ExecResult is
// returned after the agent writes its exit frame.
//
// Fails fast with ErrNotFound for unknown IDs, or a clear error when
// the sandbox has no agent client (e.g. test stubs). Cancelling ctx
// terminates the command on the guest side.
func (m *Manager) Exec(
	ctx context.Context,
	id string,
	req agentwire.ExecRequest,
	stdout, stderr io.Writer,
) (agentwire.ExecResult, error) {
	s, err := m.Get(id)
	if err != nil {
		return agentwire.ExecResult{}, err
	}
	if s.execClient == nil {
		return agentwire.ExecResult{}, fmt.Errorf("sandbox %s has no agent vsock path", id)
	}
	return s.execClient.Exec(ctx, req, stdout, stderr)
}

// Get returns the sandbox with the given ID, or ErrNotFound.
func (m *Manager) Get(id string) (*Sandbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sandboxes[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

// List returns a snapshot of current sandboxes. The slice is a copy; the
// pointed-to Sandbox values are shared and must not be mutated by callers.
func (m *Manager) List() []*Sandbox {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Sandbox, 0, len(m.sandboxes))
	for _, s := range m.sandboxes {
		out = append(out, s)
	}
	return out
}

// Shutdown tears down every sandbox currently in the map. Errors are not
// propagated (we want to continue draining even if one sandbox misbehaves)
// — callers should rely on ctx to bound total wallclock and on the
// Manager's logging for per-sandbox failures.
//
// After Shutdown returns, the Manager is empty. It is safe to call
// concurrently with Create, but new creates that race the shutdown may
// survive until the next drain — the daemon prevents this by stopping
// the HTTP server first.
func (m *Manager) Shutdown(ctx context.Context) {
	for _, s := range m.List() {
		_ = m.Delete(ctx, s.ID)
	}
}

// Snapshot captures a frozen reference point from the given sandbox
// and returns a handle to it. The source sandbox is paused briefly
// (typically sub-second) and resumes as soon as the snapshot is on disk.
//
// Mechanics:
//
//  1. Pause the source.
//  2. Clone the source's rootfs into the snapshot dir (a frozen copy
//     bound to the snapshot's lifetime; forks get their own COW
//     clones of this file via fsutil.Clone).
//  3. handle.Snapshot — writes state + memory files to the snapshot
//     dir. The runner is responsible for any path translation
//     (jailer'd firecracker writes inside its chroot and the handle
//     moves the files out; direct firecracker writes straight to the
//     paths we give it).
//  4. Resume the source.
//
// Earlier revisions wrapped step 3 in a drive-PATCH dance (swap
// source's drive to snap-dir before CreateSnapshot, swap back after)
// so the snapshot recorded a stable rootfs path. That was dropped
// once JailerRunner landed: under jailer every VM sees the same
// chroot-relative /rootfs.ext4 so the recorded path is stable by
// construction, and under the direct runner the existing
// PATCH-after-load in Restore redirects fork rootfs writes
// regardless of what the snapshot recorded. Dropping it removed
// three API calls and a failure-rollback branch.
//
// If any step after pause fails, the source is best-effort resumed
// and the snapshot dir is removed.
func (m *Manager) Snapshot(ctx context.Context, sandboxID string) (*Snapshot, error) {
	src, err := m.Get(sandboxID)
	if err != nil {
		return nil, err
	}

	snapID, err := NewSnapshotID()
	if err != nil {
		return nil, err
	}
	snapDir := filepath.Join(m.cfg.WorkBase, snapID)
	var success bool
	defer func() {
		if !success {
			_ = os.RemoveAll(snapDir)
		}
	}()

	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return nil, fmt.Errorf("sandbox: create snapshot dir: %w", err)
	}

	srcRootfs := filepath.Join(src.Workdir, perSandboxRootfsName)
	snapRootfs := filepath.Join(snapDir, snapshotRootfsName)
	snapState := filepath.Join(snapDir, snapshotStateName)
	snapMem := filepath.Join(snapDir, snapshotMemoryName)

	if err := src.handle.Pause(ctx); err != nil {
		return nil, fmt.Errorf("sandbox: pause %s: %w", src.ID, err)
	}
	// From here on, any failure must attempt to resume the source.
	defer func() {
		if !success {
			_ = src.handle.Resume(context.Background())
		}
	}()

	if err := fsutil.Clone(srcRootfs, snapRootfs); err != nil {
		return nil, fmt.Errorf("sandbox: clone source rootfs into snapshot: %w", err)
	}
	if err := src.handle.Snapshot(ctx, snapState, snapMem); err != nil {
		return nil, fmt.Errorf("sandbox: create snapshot: %w", err)
	}
	if err := src.handle.Resume(ctx); err != nil {
		return nil, fmt.Errorf("sandbox: resume %s: %w", src.ID, err)
	}

	snap := &Snapshot{
		ID:         snapID,
		SourceID:   src.ID,
		VCPUs:      src.VCPUs,
		MemoryMiB:  src.MemoryMiB,
		Dir:        snapDir,
		StatePath:  snapState,
		MemPath:    snapMem,
		RootfsPath: snapRootfs,
		CreatedAt:  time.Now().UTC(),
	}
	// Record the source's network intent so forks reconstruct a
	// matching config. Matchers are immutable, so sharing the
	// same instance across source + forks is safe — simpler than
	// re-parsing patterns for every fork.
	if src.Network != nil && src.Network.Allowlist != nil {
		snap.Network = &NetworkConfig{
			Allowlist: src.Network.Allowlist,
		}
	}

	m.mu.Lock()
	m.snapshots[snapID] = snap
	m.mu.Unlock()

	success = true
	return snap, nil
}

// GetSnapshot returns the snapshot with the given ID or ErrSnapshotNotFound.
func (m *Manager) GetSnapshot(id string) (*Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.snapshots[id]
	if !ok {
		return nil, ErrSnapshotNotFound
	}
	return s, nil
}

// ListSnapshots returns a snapshot of currently-registered snapshots.
func (m *Manager) ListSnapshots() []*Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Snapshot, 0, len(m.snapshots))
	for _, s := range m.snapshots {
		out = append(out, s)
	}
	return out
}

// DeleteSnapshot removes the snapshot's registry entry and its on-disk
// files. Forks already created from it are unaffected — they have their
// own per-fork rootfs and memory copies.
func (m *Manager) DeleteSnapshot(ctx context.Context, id string) error {
	m.mu.Lock()
	snap, ok := m.snapshots[id]
	if !ok {
		m.mu.Unlock()
		return ErrSnapshotNotFound
	}
	delete(m.snapshots, id)
	m.mu.Unlock()

	_ = ctx // reserved for future cancellation bounds on large deletes
	if err := os.RemoveAll(snap.Dir); err != nil {
		return fmt.Errorf("sandbox: remove snapshot dir %s: %w", snap.Dir, err)
	}
	return nil
}

// Fork creates `count` new sandboxes from a snapshot. Each fork gets
// its own workdir, per-fork memory file (cloned from the snapshot's),
// and per-fork rootfs (cloned from the snapshot's frozen rootfs).
//
// Forks run in parallel. The dominant per-fork cost on a filesystem
// without reflink support is byte-copying the memory file (~0.5 GB)
// and rootfs (~1 GB), which is I/O-bound and parallelizes well; the
// non-I/O portions (jailer staging, LoadSnapshot, vCPU restore) are
// independent. On a test host we measured ~2.1s/fork serially vs
// ~1.0s/fork in parallel (count=3).
//
// If any fork in the batch fails to start, every fork that
// succeeded is torn down before returning — Fork remains
// all-or-nothing. The first observed error is returned (indexed by
// its fork position for readability).
func (m *Manager) Fork(ctx context.Context, snapshotID string, count int) ([]*Sandbox, error) {
	if count <= 0 {
		return nil, errors.New("sandbox: Fork count must be > 0")
	}
	snap, err := m.GetSnapshot(snapshotID)
	if err != nil {
		return nil, err
	}

	type forkResult struct {
		sb  *Sandbox
		err error
	}

	results := make([]forkResult, count)
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func(idx int) {
			defer wg.Done()
			sb, err := m.forkOne(ctx, snap)
			results[idx] = forkResult{sb: sb, err: err}
		}(i)
	}
	wg.Wait()

	// Reassemble: preserve the caller-requested ordering, collect the
	// first error (if any), and build the success list.
	forks := make([]*Sandbox, 0, count)
	var firstErr error
	var firstErrIdx int
	for i, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				firstErrIdx = i
			}
			continue
		}
		forks = append(forks, r.sb)
	}

	if firstErr != nil {
		// Roll back successful forks so a partial batch doesn't leak.
		// Use Background so a cancelled caller ctx doesn't prevent the
		// teardown from running.
		for _, sb := range forks {
			_ = m.Delete(context.Background(), sb.ID)
		}
		return nil, fmt.Errorf("sandbox: fork %d/%d: %w", firstErrIdx+1, count, firstErr)
	}

	return forks, nil
}

// forkOne creates a single fork. Split out so Fork's all-or-nothing
// loop stays readable.
func (m *Manager) forkOne(ctx context.Context, snap *Snapshot) (*Sandbox, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	workdir := filepath.Join(m.cfg.WorkBase, id)

	var success bool
	defer func() {
		if !success {
			_ = os.RemoveAll(workdir)
		}
	}()

	if err := os.MkdirAll(workdir, 0o750); err != nil {
		return nil, fmt.Errorf("create workdir: %w", err)
	}

	forkRootfs := filepath.Join(workdir, perSandboxRootfsName)
	forkMem := filepath.Join(workdir, perForkMemoryName)

	if err := fsutil.Clone(snap.RootfsPath, forkRootfs); err != nil {
		return nil, fmt.Errorf("clone snapshot rootfs: %w", err)
	}
	if err := fsutil.Clone(snap.MemPath, forkMem); err != nil {
		return nil, fmt.Errorf("clone snapshot memory: %w", err)
	}

	// Network for the fork, if the source had one. The
	// provisioner gives us a fresh netns + subnet + MAC even
	// though the allowlist (policy) is the same as the source.
	netHandle, err := m.provisionNetwork(ctx, id, snap.Network)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success && netHandle != nil {
			_ = m.cfg.Network.Teardown(context.Background(), netHandle)
		}
	}()

	restoreSpec := runner.RestoreSpec{
		Workdir:    workdir,
		StatePath:  snap.StatePath,
		MemPath:    forkMem,
		RootfsPath: forkRootfs,
	}
	if netHandle != nil {
		restoreSpec.NetNS = netHandle.NetnsPath
	}

	handle, err := m.cfg.Runner.Restore(ctx, restoreSpec)
	if err != nil {
		return nil, fmt.Errorf("runner restore: %w", err)
	}

	s := &Sandbox{
		ID:        id,
		VCPUs:     snap.VCPUs,
		MemoryMiB: snap.MemoryMiB,
		Workdir:   workdir,
		VSockPath: handle.VSockPath(),
		CreatedAt: time.Now().UTC(),
		Network:   netHandle,
		handle:    handle,
		done:      make(chan struct{}),
	}
	if s.VSockPath != "" {
		s.execClient = agentapi.NewClient(s.VSockPath, agentwire.AgentVSockPort)
	}

	// On a restored VM the agent is already running inside — its
	// listener survived the snapshot. WaitForAgent is typically
	// unnecessary for forks, but we honor the setting for consistency
	// with Create.
	if m.cfg.WaitForAgent && s.execClient != nil {
		if err := m.waitForAgent(ctx, s.execClient); err != nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("agent not ready after restore: %w", err)
		}
	}

	// If the fork has network, ask the guest to bounce eth0 so
	// systemd-networkd runs a fresh DHCP cycle and picks up the
	// fork's per-netns-assigned IP. The guest's kernel restored
	// from a snapshot where eth0 holds the source's IP; without
	// this, the fork is dark until systemd-networkd's next
	// renewal cycle. Failures here are non-fatal — the fork
	// still boots, it just has a slower first DHCP.
	if netHandle != nil && s.execClient != nil {
		refreshCtx, cancel := context.WithTimeout(ctx, networkRefreshTimeout)
		if err := s.execClient.RefreshNetwork(refreshCtx); err != nil {
			// Log-and-continue: the guest recovers on its own
			// within one DHCP renewal cycle (~30s given our 60s
			// lease).
			_ = err
		}
		cancel()
	}

	m.mu.Lock()
	m.sandboxes[id] = s
	m.mu.Unlock()

	success = true
	return s, nil
}

// networkRefreshTimeout bounds the post-resume RefreshNetwork RPC.
// Must be at least as long as the agent's own internal timeout
// (10s for the down→up→wait dance) plus slack for the vsock
// round-trip.
const networkRefreshTimeout = 15 * time.Second

// Delete shuts the sandbox down and removes it from the manager. It is
// idempotent: deleting an unknown ID returns ErrNotFound; deleting twice
// succeeds only the first time.
//
// Order matters: we remove the record from the map first so concurrent
// Gets won't observe a half-destroyed sandbox, then shut down the Handle,
// then remove the workdir from disk.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	s, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.sandboxes, id)
	m.mu.Unlock()

	// Signal any lifetime-timer goroutine to exit before we block on
	// shutdown. Safe to call more than once; doneOnce guards the close.
	s.doneOnce.Do(func() { close(s.done) })

	shutdownErr := s.handle.Shutdown(ctx)

	// Tear down the network whether or not Shutdown cleanly exited —
	// leaving netns/nft/DHCP state behind on a failed shutdown would
	// block future Create calls that want the same subnet. The
	// provisioner's Teardown is idempotent + best-effort.
	if s.Network != nil && m.cfg.Network != nil {
		if err := m.cfg.Network.Teardown(ctx, s.Network); err != nil {
			// Log-equivalent: we don't fail Delete on teardown
			// errors because we've already committed to removing
			// the sandbox from the registry. Operators see the
			// warning in the daemon log.
			_ = err // logged by the network.Manager itself
		}
	}

	if shutdownErr != nil {
		_ = os.RemoveAll(s.Workdir)
		return fmt.Errorf("sandbox: shutdown %s: %w", id, shutdownErr)
	}
	if err := os.RemoveAll(s.Workdir); err != nil {
		return fmt.Errorf("sandbox: remove workdir %s: %w", s.Workdir, err)
	}
	return nil
}
