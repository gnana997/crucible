package fcapi

import "context"

// SnapshotType controls what goes into the snapshot. v0.1 only uses
// Full snapshots; "Diff" snapshots require a prior Full + the same
// backing memory file and are a v0.2+ optimization.
type SnapshotType string

const (
	SnapshotTypeFull SnapshotType = "Full"
	SnapshotTypeDiff SnapshotType = "Diff"
)

// SnapshotCreate is the body for PUT /snapshot/create. The VM must be
// Paused before calling — Firecracker returns 400 otherwise.
//
// Firecracker writes two files:
//   - SnapshotPath: a small state file (~KB) with CPU regs, device
//     config, MMIO state, vsock state.
//   - MemPath: the guest's memory dumped to disk (equal in size to
//     machine_config.mem_size_mib). The biggest artifact.
//
// Both files must be writable by the firecracker process user and live
// on a filesystem with enough free space.
type SnapshotCreate struct {
	SnapshotType SnapshotType `json:"snapshot_type,omitempty"`
	SnapshotPath string       `json:"snapshot_path"`
	MemPath      string       `json:"mem_file_path"`
}

// CreateSnapshot dumps the current VM state and memory to the paths in
// cfg. Blocks until the files are written.
func (c *Client) CreateSnapshot(ctx context.Context, cfg SnapshotCreate) error {
	return c.do(ctx, "PUT", "/snapshot/create", cfg, nil)
}

// MemBackendType enumerates how Firecracker reads the memory file on
// load. v0.1 uses File (eager file-backed load); UffdHandler (lazy
// userfaultfd-based loading) is a v0.3 optimization.
type MemBackendType string

const (
	MemBackendFile        MemBackendType = "File"
	MemBackendUffdHandler MemBackendType = "Uffd"
)

// MemBackend wires the memory file into the restoring VMM. Keep it
// opaque in SnapshotLoad since Firecracker accepts both the legacy
// mem_file_path shorthand and this structured form — we always use
// the structured form for clarity.
type MemBackend struct {
	BackendType MemBackendType `json:"backend_type"`
	BackendPath string         `json:"backend_path"`
}

// SnapshotLoad is the body for PUT /snapshot/load. Invoke on a fresh
// firecracker process (never on one that already has a VM running).
// Before calling, PUT /vsock if the snapshotted VM had one — the vsock
// UDS path in the loading side overrides what was captured.
//
// ResumeVM=true auto-resumes after load (one fewer round-trip than
// loading Paused and calling PUT /vm{state:Resumed}).
//
// EnableDiffSnapshots should be false for v0.1 — we never write diff
// snapshots, so there's no base to combine with.
type SnapshotLoad struct {
	SnapshotPath        string     `json:"snapshot_path"`
	MemBackend          MemBackend `json:"mem_backend"`
	EnableDiffSnapshots bool       `json:"enable_diff_snapshots,omitempty"`
	ResumeVM            bool       `json:"resume_vm,omitempty"`
}

// LoadSnapshot restores a previously-created snapshot into this VMM
// process. Returns once the VM is loaded (and resumed, if ResumeVM).
func (c *Client) LoadSnapshot(ctx context.Context, cfg SnapshotLoad) error {
	return c.do(ctx, "PUT", "/snapshot/load", cfg, nil)
}
