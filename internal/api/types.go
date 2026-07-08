// Package api holds the crucible daemon's REST wire types — the request
// and response shapes exchanged over HTTP. They are pure data with no
// behavior and no dependencies beyond the standard library and
// internal/agentwire (itself pure wire data, shared for the exec and
// service contracts), so both the daemon (server) and internal/client
// (and, later, the TUI, an MCP server, and the SDK) can share one
// source of truth for the contract.
//
// Validation logic lives in the daemon, not here: keeping these types
// dependency-free is what lets a client import them without pulling in
// the server's guts.
package api

import (
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

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

	// Service, when non-nil, configures and starts a supervised
	// long-lived entrypoint in the guest right after the agent is ready
	// — the sandbox arrives with the service already running, or the
	// create fails. EXPERIMENTAL: the field shape (agentwire.ServiceSpec,
	// same package that carries the exec contract) may still change
	// before it is declared stable. Pair with timeout_s = 0 for a
	// service that outlives the default sandbox lifetime.
	Service *agentwire.ServiceSpec `json:"service,omitempty"`

	// Publish maps host ports to guest ports so a service inside the
	// sandbox is reachable from the host (and, by default, the LAN) —
	// the moral equivalent of `docker run -p`. Publishing requires the
	// sandbox to have a NIC; when Network is absent it is created with
	// an empty egress allowlist (ingress-published, egress-denied).
	Publish []PortMapping `json:"publish,omitempty"`

	// Pull controls image acquisition when Image.OCI is set, mirroring
	// `docker run --pull`: "missing" (default, and the empty value)
	// converts + caches the image on a store miss so a bare `--image
	// nginx:alpine` Just Works; "always" re-pulls even on a store hit;
	// "never" fails if the image isn't already converted. Ignored when
	// Image is unset.
	Pull string `json:"pull,omitempty"`
}

// PortMapping publishes one host port to one guest port.
type PortMapping struct {
	// HostIP is the host address to bind. Empty means 0.0.0.0 (reachable
	// from the LAN); "127.0.0.1" pins it to localhost-only.
	HostIP string `json:"host_ip,omitempty"`

	// HostPort is the port on the host; GuestPort is the port inside the
	// sandbox the connection is forwarded to. Both 1..65535.
	HostPort  int `json:"host_port"`
	GuestPort int `json:"guest_port"`

	// Protocol is "tcp" (the default and, for now, only supported value).
	Protocol string `json:"protocol,omitempty"`
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
	ID        string    `json:"id"`
	VCPUs     int       `json:"vcpus"`
	MemoryMiB int       `json:"memory_mib"`
	Workdir   string    `json:"workdir"`
	Profile   string    `json:"profile,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// SourceSnapshotID is the snapshot this sandbox was forked from; absent for
	// a directly-created sandbox. Together with SnapshotResponse.SourceID it
	// carries the fork lineage a client needs to render a fork tree.
	SourceSnapshotID string `json:"source_snapshot_id,omitempty"`

	Network *NetworkResponse `json:"network,omitempty"`

	// Published echoes the applied host→guest port mappings.
	Published []PortMapping `json:"published,omitempty"`
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

// LogRecord is one durable log line returned by GET /sandboxes/{id}/logs.
// Source is "service" (the entrypoint's output) or "exec" (an exec
// invocation); Stream is "stdout", "stderr", or "event" (a synthesized
// line such as an exec start/exit or a log-ring gap).
type LogRecord struct {
	TimeMs int64  `json:"time_ms"`
	Source string `json:"source"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// LogsResponse is the JSON body of GET /sandboxes/{id}/logs. NextOffset is
// the byte cursor to pass as `since` on the next read to follow the log.
type LogsResponse struct {
	Records    []LogRecord `json:"records"`
	NextOffset int64       `json:"next_offset"`
}

// PullImageRequest is the JSON body of POST /images.
type PullImageRequest struct {
	// Ref is the image reference to pull (e.g. "nginx:latest",
	// "ghcr.io/org/app:v1"). Required.
	Ref string `json:"ref"`
}

// ImageResponse describes one converted image in the store.
type ImageResponse struct {
	Digest       string `json:"digest"`
	SourceRef    string `json:"source_ref,omitempty"`
	SizeBytes    int64  `json:"size_bytes"`
	ContentBytes int64  `json:"content_bytes"`
	Entries      int    `json:"entries"`
	ConvertMode  string `json:"convert_mode,omitempty"`
	ConvertedAt  int64  `json:"converted_at_unix_ms,omitempty"`

	// Entrypoint and Cmd echo the image's runtime contract so a client
	// can see what a sandbox created from this image would run.
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`

	// ExposedPorts are the image's declared ports ("8080/tcp"), a hint
	// for future ingress defaults.
	ExposedPorts []string `json:"exposed_ports,omitempty"`
}

// ImageListResponse wraps the image list.
type ImageListResponse struct {
	Images []ImageResponse `json:"images"`
}

// ErrorResponse is the body of any non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}
