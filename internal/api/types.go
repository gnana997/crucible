// Package api holds the crucible daemon's REST wire types — the request
// and response shapes exchanged over HTTP. They are pure data with no
// behavior and no dependencies beyond the standard library, so both the
// daemon (server) and internal/client (and, later, the TUI, an MCP
// server, and the SDK) can share one source of truth for the contract.
//
// Validation logic lives in the daemon, not here: keeping these types
// dependency-free is what lets a client import them without pulling in
// the server's guts.
package api

import "time"

// CreateSandboxRequest is the JSON body for POST /sandboxes. All fields
// are optional; the daemon fills in defaults for zero values.
type CreateSandboxRequest struct {
	VCPUs     int    `json:"vcpus,omitempty"`
	MemoryMiB int    `json:"memory_mib,omitempty"`
	BootArgs  string `json:"boot_args,omitempty"`

	// TimeoutSec sets a maximum lifetime for the sandbox in seconds.
	// Zero means no timeout; the sandbox lives until an explicit
	// DELETE or daemon shutdown.
	TimeoutSec int `json:"timeout_s,omitempty"`

	// Profile names a pre-baked rootfs the daemon boots this sandbox
	// from (e.g. "python-3.12", "node-22", "base"). Empty uses the
	// daemon's default --rootfs. Resolved against the daemon's
	// --rootfs-dir; an unknown profile is a 400.
	Profile string `json:"profile,omitempty"`

	// Image, when set, overrides the daemon's default rootfs for this
	// sandbox. WIRE CONTRACT LOCK: the shape is frozen so the OCI client
	// can land without a breaking API change. In v0.1 both Path and OCI
	// return 501 — leave Image unset and rely on the daemon --rootfs /
	// profiles.
	Image *ImageRef `json:"image,omitempty"`

	// Network, when non-nil, attaches a NIC with an explicit allowlist
	// of hostnames the guest can reach. Nil means no network
	// (default-deny).
	Network *NetworkRequest `json:"network,omitempty"`
}

// NetworkRequest is the per-sandbox network policy on the wire.
// See docs/network.md for the full semantics.
type NetworkRequest struct {
	// Enabled = false (or the field absent) is treated as "no network".
	// When true, Allowlist must be non-empty.
	Enabled bool `json:"enabled"`

	// Allowlist is the hostname-pattern list enforced by the host-side
	// DNS proxy + nftables. Each entry is an exact hostname ("pypi.org")
	// or a single-label wildcard ("*.npmjs.org"). Bare "*" is rejected.
	Allowlist []string `json:"allowlist,omitempty"`
}

// ImageRef identifies a per-sandbox rootfs override. Exactly one of
// Path / OCI must be set. Reserved in v0.1 — any value returns 501.
type ImageRef struct {
	Path string `json:"path,omitempty"`
	OCI  string `json:"oci,omitempty"`
}

// SandboxResponse is the JSON shape returned for a single sandbox.
type SandboxResponse struct {
	ID        string           `json:"id"`
	VCPUs     int              `json:"vcpus"`
	MemoryMiB int              `json:"memory_mib"`
	Workdir   string           `json:"workdir"`
	Profile   string           `json:"profile,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	Network   *NetworkResponse `json:"network,omitempty"`
}

// NetworkResponse is the applied network policy echoed back after Create.
// Nil when the sandbox has no NIC.
type NetworkResponse struct {
	Enabled   bool     `json:"enabled"`
	GuestIP   string   `json:"guest_ip,omitempty"`
	Gateway   string   `json:"gateway,omitempty"`
	Allowlist []string `json:"allowlist,omitempty"`
}

// ListResponse wraps the sandbox list so the shape can grow without
// breaking clients (e.g. adding "next_page" later).
type ListResponse struct {
	Sandboxes []SandboxResponse `json:"sandboxes"`
}

// SnapshotResponse is the JSON shape returned for a single snapshot. The
// on-disk paths are included for operator debugging and leak no secrets.
type SnapshotResponse struct {
	ID         string    `json:"id"`
	SourceID   string    `json:"source_id"`
	VCPUs      int       `json:"vcpus"`
	MemoryMiB  int       `json:"memory_mib"`
	Dir        string    `json:"dir"`
	StatePath  string    `json:"state_path"`
	MemPath    string    `json:"mem_path"`
	RootfsPath string    `json:"rootfs_path"`
	CreatedAt  time.Time `json:"created_at"`
}

// SnapshotListResponse wraps the snapshot list.
type SnapshotListResponse struct {
	Snapshots []SnapshotResponse `json:"snapshots"`
}

// ForkResponse is returned by POST /snapshots/{id}/fork.
type ForkResponse struct {
	Sandboxes []SandboxResponse `json:"sandboxes"`
}

// ProfilesResponse is returned by GET /profiles: the profile names the
// daemon has configured via --rootfs-dir, sorted.
type ProfilesResponse struct {
	Profiles []string `json:"profiles"`
}

// ErrorResponse is the body of any non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}
