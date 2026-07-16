// Package api holds the crucible daemon's REST wire types — the request
// and response shapes exchanged over HTTP. They are pure data with no
// behavior and no dependencies beyond the standard library and
// sdk/wire (itself pure wire data, shared for the exec and
// service contracts), so the daemon (server) and the SDK client
// (package crucible, one directory up) — and the CLI, TUI, and MCP
// server built on it — share one source of truth for the contract.
//
// Validation logic lives in the daemon, not here: keeping these types
// dependency-free is what lets a client import them without pulling in
// the server's guts.
package api

import (
	"time"

	"github.com/gnana997/crucible/sdk/wire"
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
	// create fails. EXPERIMENTAL: the field shape (wire.ServiceSpec,
	// same package that carries the exec contract) may still change
	// before it is declared stable. Pair with timeout_s = 0 for a
	// service that outlives the default sandbox lifetime.
	Service *wire.ServiceSpec `json:"service,omitempty"`

	// Publish maps host ports to guest ports so a service inside the
	// sandbox is reachable from the host (and, by default, the LAN) —
	// the moral equivalent of `docker run -p`. Publishing requires the
	// sandbox to have a NIC; when Network is absent it is created with
	// an empty egress allowlist (ingress-published, egress-denied).
	Publish []PortMapping `json:"publish,omitempty"`

	// PublishAll publishes every port the image declares with EXPOSE,
	// each to the same host port number (guest N → host N) — the
	// deterministic analogue of `docker run -P`. Only meaningful with an
	// OCI Image (profiles have no EXPOSE metadata); tcp ports only. An
	// explicit Publish entry for a guest port wins over the auto-mapping.
	PublishAll bool `json:"publish_all,omitempty"`

	// Pull controls image acquisition when Image.OCI is set, mirroring
	// `docker run --pull`: "missing" (default, and the empty value)
	// converts + caches the image on a store miss so a bare `--image
	// nginx:alpine` Just Works; "always" re-pulls even on a store hit;
	// "never" fails if the image isn't already converted. Ignored when
	// Image is unset.
	Pull string `json:"pull,omitempty"`

	// RegistryAuth is an optional one-shot credential for pulling Image from
	// a private registry (never stored). Overrides any stored credential for
	// that registry, for this pull only. Ignored when the image is resolved
	// from the store (no pull) or side-loaded from local docker.
	RegistryAuth *RegistryAuth `json:"registry_auth,omitempty"`

	// DiskBytes, when > 0, grows this sandbox's writable rootfs to at
	// least this many bytes. The daemon grows the per-sandbox clone
	// (truncate + resize2fs) after cloning the template — the cached
	// image/profile ext4 is never touched. Zero keeps the template's
	// built-in headroom. Ignored (a no-op) when the requested size is
	// not larger than the clone already is.
	DiskBytes int64 `json:"disk_bytes,omitempty"`

	// Volumes are persistent block-device volumes to attach and mount at a
	// path inside the guest, e.g. {Name: "pgdata", Path:
	// "/var/lib/postgresql/data"}. Each is created + formatted ext4 on first
	// use and reattached by name thereafter, so data survives the sandbox
	// (and daemon restarts). Requires the daemon to have a volume directory.
	Volumes []VolumeMount `json:"volumes,omitempty"`

	// Env adds environment variables to the sandbox's entrypoint (the image
	// ENTRYPOINT/CMD or an explicit Service), docker `-e` style: these win
	// over the image's ENV. It applies only when the sandbox has an entrypoint
	// service; a bare profile sandbox with no long-lived process gets its env
	// per-command via ExecRequest.Env instead.
	Env map[string]string `json:"env,omitempty"`
}

// VolumeMount attaches a durable, named block-device volume at an absolute
// path inside the guest. The backing store persists across sandboxes, so
// re-creating with the same Name reattaches the same data. ext4 is
// single-writer: a volume may be attached to at most one live sandbox.
type VolumeMount struct {
	// Name is the durable volume name ([a-z0-9][a-z0-9-]*, max 63 chars).
	Name string `json:"name"`
	// Path is the absolute mount point inside the guest.
	Path string `json:"path"`
}

// Volume is a persistent block-device volume managed by the daemon.
type Volume struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	CreatedAt  time.Time `json:"created_at"`
	HostID     string    `json:"host_id,omitempty"`     // host this volume is pinned to
	AttachedTo string    `json:"attached_to,omitempty"` // sandbox id, empty if detached
	Encrypted  bool      `json:"encrypted,omitempty"`   // per-volume LUKS encryption at rest
	KeyID      string    `json:"key_id,omitempty"`      // keyring id whose key wraps this volume (encrypted only)
}

// RewrapVolumeRequest is the body of POST /volumes/{name}/rewrap: re-wrap the
// volume's key under keyring key ToKeyID (rotation; no data is re-encrypted).
type RewrapVolumeRequest struct {
	ToKeyID string `json:"to_key_id"`
}

// BulkRewrapRequest is the body of POST /volumes/rewrap: re-wrap every volume
// currently on FromKeyID to ToKeyID.
type BulkRewrapRequest struct {
	FromKeyID string `json:"from_key_id"`
	ToKeyID   string `json:"to_key_id"`
}

// BulkRewrapResponse reports how many volumes a bulk rewrap re-keyed.
type BulkRewrapResponse struct {
	Rewrapped int `json:"rewrapped"`
}

// CreateVolumeRequest is the body of POST /volumes.
type CreateVolumeRequest struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes,omitempty"` // 0 = daemon default
	// Encrypt overrides the daemon's --volume-encrypt default for this volume:
	// nil = use the default, non-nil = force on/off. Requires a master key.
	Encrypt *bool `json:"encrypt,omitempty"`
}

// VolumeListResponse is the body of GET /volumes.
type VolumeListResponse struct {
	Volumes []Volume `json:"volumes"`
}

// Backup is a point-in-time copy of a volume, restorable to a new volume.
type Backup struct {
	ID           string    `json:"id"`
	SourceVolume string    `json:"source_volume"`
	SizeBytes    int64     `json:"size_bytes"`
	CreatedAt    time.Time `json:"created_at"`
	Consistency  string    `json:"consistency,omitempty"` // "filesystem"
	HostID       string    `json:"host_id,omitempty"`
	Encrypted    bool      `json:"encrypted,omitempty"` // backup of an encrypted volume (ciphertext)
}

// BackupListResponse is the body of GET /backups and GET /volumes/{name}/backups.
type BackupListResponse struct {
	Backups []Backup `json:"backups"`
}

// RestoreVolumeRequest is the body of POST /volumes/{name}/restore: create the
// new volume {name} from backup From.
type RestoreVolumeRequest struct {
	From string `json:"from"` // backup id
}

// CloneVolumeRequest is the body of POST /volumes/{name}/clone: copy the
// (quiescent) volume {name} into a new volume To.
type CloneVolumeRequest struct {
	To string `json:"to"` // new volume name
}

// GrowVolumeRequest is the body of POST /volumes/{name}/grow: enlarge the
// volume's backing store + filesystem to SizeBytes. Grow-only — a value at or
// below the current size is rejected.
type GrowVolumeRequest struct {
	SizeBytes int64 `json:"size_bytes"` // new total size in bytes
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

	// FullEgress, when true, lets the guest reach any *public* host —
	// "the internet", but still with metadata/link-local/RFC1918/CGNAT
	// and the other reserved ranges blocked (public-unicast only). It is
	// the right default for a trusted app you deploy yourself; it does NOT
	// weaken the SSRF guard. Composes with Allowlist/AllowlistCIDR (it is
	// the broadest of the three). Requires enabled=true.
	FullEgress bool `json:"full_egress,omitempty"`

	// AllowlistCIDR permits direct egress to IP literals inside these
	// IPv4 prefixes (e.g. "203.0.113.0/24"), which the hostname allowlist
	// alone can't express. Each prefix is still filtered to public-unicast
	// space: a prefix overlapping private/metadata ranges has those
	// addresses dropped, and a wholly-private prefix reaches nothing.
	AllowlistCIDR []string `json:"allowlist_cidr,omitempty"`
}

// ImageRef identifies a per-sandbox rootfs override. Exactly one of
// Path / OCI must be set. OCI names a registry image (pulled and
// converted on demand, then cached); Path is a prepared rootfs on the
// daemon host. A daemon without an image store answers 501.
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

// ForkRequest is the optional JSON body of POST /snapshots/{id}/fork.
// Both fields are optional; count may also be given as the ?count query
// parameter (the body wins when both are set).
type ForkRequest struct {
	// Count is how many sandboxes to fork (default 1).
	Count int `json:"count,omitempty"`

	// Publish maps host ports to guest ports on the forked sandbox,
	// exactly like CreateSandboxRequest.Publish. Only valid with
	// count 1: host ports are exclusive, so a fan-out cannot share
	// them. Requires a daemon >= v0.3.4; older daemons ignore the
	// request body entirely.
	Publish []PortMapping `json:"publish,omitempty"`
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

	// RegistryAuth is an optional one-shot credential for this pull only
	// (never stored). Overrides any stored credential for the ref's registry.
	RegistryAuth *RegistryAuth `json:"registry_auth,omitempty"`
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
	// Code is a stable, machine-readable error code for the cases a
	// programmatic client needs to branch on (name collisions, in-use
	// conflicts, lifecycle-state mismatches). Empty for errors with no
	// dedicated code. See the Code* constants.
	Code string `json:"code,omitempty"`
}

// Error codes returned in ErrorResponse.Code. Stable strings a client can
// switch on without matching human-readable messages. Empty means "no
// dedicated code" (branch on the HTTP status instead).
const (
	CodeNameTaken        = "name_taken"         // 409: an app/volume of that name exists
	CodeInUse            = "in_use"             // 409: volume attached to a live sandbox
	CodeNotFound         = "not_found"          // 404: no such resource
	CodeNotRunning       = "not_running"        // 409: no running instance (e.g. sleep)
	CodeNotAsleep        = "not_asleep"         // 409: not asleep (e.g. wake)
	CodeInvalidName      = "invalid_name"       // 400: name failed validation
	CodeInvalidConfig    = "invalid_config"     // 400: bad sandbox/app config
	CodeSnapshotNotFound = "snapshot_not_found" // 404: no such snapshot
	CodeBackupNotFound   = "backup_not_found"   // 404: no such backup
)
