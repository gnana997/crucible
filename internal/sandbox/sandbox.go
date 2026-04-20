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
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/runner"
)

// Default machine sizing applied when a CreateConfig leaves a field at 0.
// These mirror the "sane defaults" principle from docs/VISION.md — small
// enough to be cheap, big enough to run a Python interpreter.
const (
	DefaultVCPUs     = 1
	DefaultMemoryMiB = 512
)

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
	CreatedAt time.Time

	handle runner.Handle
}

// CreateConfig is the input to Manager.Create. Zero-valued fields are
// filled in with package defaults.
type CreateConfig struct {
	VCPUs     int
	MemoryMiB int
	BootArgs  string // empty means use runner.DefaultBootArgs
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
	Kernel string
	Rootfs string
}

// Manager owns the sandbox map and coordinates lifecycle operations.
type Manager struct {
	cfg ManagerConfig

	mu        sync.RWMutex
	sandboxes map[string]*Sandbox
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
	}, nil
}

// Create allocates a new sandbox, boots its VM, and stores the record.
// If the runner fails to start, no record is stored and the error is
// returned; callers can safely retry.
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

	handle, err := m.cfg.Runner.Start(ctx, runner.Spec{
		Workdir:   workdir,
		Kernel:    m.cfg.Kernel,
		Rootfs:    m.cfg.Rootfs,
		BootArgs:  req.BootArgs,
		VCPUs:     vcpus,
		MemoryMiB: memMiB,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: start %s: %w", id, err)
	}

	s := &Sandbox{
		ID:        id,
		VCPUs:     vcpus,
		MemoryMiB: memMiB,
		Workdir:   workdir,
		CreatedAt: time.Now().UTC(),
		handle:    handle,
	}

	m.mu.Lock()
	m.sandboxes[id] = s
	m.mu.Unlock()
	return s, nil
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

	if err := s.handle.Shutdown(ctx); err != nil {
		// Best-effort workdir cleanup even if shutdown reported an error
		// (the process may have been killed).
		_ = os.RemoveAll(s.Workdir)
		return fmt.Errorf("sandbox: shutdown %s: %w", id, err)
	}
	if err := os.RemoveAll(s.Workdir); err != nil {
		return fmt.Errorf("sandbox: remove workdir %s: %w", s.Workdir, err)
	}
	return nil
}
