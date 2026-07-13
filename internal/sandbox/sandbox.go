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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/agentapi"
	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/fsutil"
	"github.com/gnana997/crucible/internal/metrics"
	"github.com/gnana997/crucible/internal/runner"
	"github.com/gnana997/crucible/sdk/wire"
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

// Per-request resource ceilings. crucible is a multi-tenant node running
// untrusted code, so a single (unauthenticated) request must not be able
// to reserve the whole host. These bound the create-time sizing and the
// fork fan-out at the Manager boundary; the HTTP layer rejects violations
// as 400 rather than surfacing an opaque Firecracker 500 or OOM-killing
// the daemon.
const (
	// MaxVCPUs caps per-sandbox vCPUs. Firecracker itself rejects absurd
	// counts, but we bound it here for a clean error and to leave the host
	// headroom.
	MaxVCPUs = 32
	// MaxMemoryMiB caps per-sandbox guest memory (64 GiB). A larger value
	// flows straight into PUT /machine-config → mmap and can exhaust host
	// memory.
	MaxMemoryMiB = 64 * 1024
	// DefaultMaxForkCount bounds how many sandboxes a single Fork request
	// may create when ManagerConfig.MaxForkCount is left at 0. Fork
	// allocates and spawns proportional to count before the concurrency
	// semaphore throttles anything, so an unbounded count OOMs the daemon.
	DefaultMaxForkCount = 64
)

// Snapshot/restore deadline budget. A snapshot or restore's dominant cost
// is writing/reading the guest memory file, which scales with guest size
// and, on non-NVMe storage, can run well past a minute (a 4 GiB guest
// already blows a 10s budget). The fcapi client carries no wall-clock cap
// (so a large guest can complete) and the HTTP request ctx carries no
// deadline, so these size a per-operation ctx to guest memory: a
// legitimately large guest completes, while a wedged firecracker still
// can't hang the daemon goroutine forever.
const (
	snapshotBaseTimeout   = 30 * time.Second
	snapshotTimeoutPerMiB = 20 * time.Millisecond
)

// snapshotTimeout returns the deadline budget for a snapshot or restore of
// a guest with the given memory size.
func snapshotTimeout(memoryMiB int) time.Duration {
	d := snapshotBaseTimeout
	if memoryMiB > 0 {
		d += time.Duration(memoryMiB) * snapshotTimeoutPerMiB
	}
	return d
}

// resumeRollbackTimeout bounds the best-effort Resume that unpauses the
// source after a failed snapshot. The caller ctx is unusable there — it may
// be why the snapshot failed, and is cancelled once the sized budget above
// expires — so the rollback runs on a fresh, bounded ctx, mirroring the
// Shutdown grace: a wedged firecracker must not hang the Snapshot
// goroutine forever with an unbounded Resume(context.Background()).
const resumeRollbackTimeout = 10 * time.Second

// ErrInvalidConfig marks a request that violates a resource bound (vCPUs,
// memory, or fork count). The HTTP layer maps it to 400.
var ErrInvalidConfig = errors.New("sandbox: invalid config")

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
	Profile   string // rootfs profile this sandbox booted from; empty = default rootfs
	TokenID   string // id of the API key that created it; "" when unauthenticated. Used for per-token max_sandboxes.
	VSockPath string // host UDS for Firecracker's hybrid vsock; empty for test stubs
	CreatedAt time.Time

	// StaticNetwork is true for OCI-image guests: they have no DHCP
	// client, so the daemon pushes the network config over vsock at
	// create and re-pushes on fork (instead of the DHCP link-bounce).
	StaticNetwork bool

	// SourceSnapshotID is the snapshot this sandbox was forked from; empty for
	// a directly-created (non-forked) sandbox. It carries the fork lineage so a
	// client can reconstruct the fork tree (snapshot.SourceID → sandbox →
	// snapshot → forked sandbox).
	SourceSnapshotID string

	// Network is nil when the sandbox has no NIC attached (default).
	// Non-nil carries everything the daemon needs to teardown and
	// to echo back in sandboxResponse.
	Network *NetworkHandle

	// Published are the host→guest port mappings applied at create,
	// echoed back in sandboxResponse. Nil when nothing was published.
	Published []PortMapping

	handle     runner.Handle
	execClient *agentapi.Client // cached; nil when VSockPath is empty
	publish    PublishHandle    // active forwarders; nil when nothing published

	// asleep, when non-nil, holds the snapshot artifacts captured by
	// SleepInPlace to wake from: the VMM is stopped (RAM freed) but the record,
	// netns, and workdir are kept. Cleared by WakeInPlace. In-memory only —
	// surviving a daemon restart while asleep is handled by the durable snapshot.
	asleep *sleepState

	// memSnapshotID is the id of the DURABLE snapshot backing this instance's
	// memory: set when the sandbox is slept (a real, journaled snapshot under
	// WorkBase, so a slept app survives a daemon restart via re-adoption), and
	// KEPT across an in-place wake because the woken VM's lazy-memory (uffd)
	// pager serves from that snapshot's memory file for its whole life. GC'd only
	// once superseded — by the next sleep (after the VMM, and its pager, is
	// stopped) or by Delete — so exactly one snapshot is kept per instance.
	memSnapshotID string

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

	// Profile names a pre-baked rootfs to boot from, resolved against
	// ManagerConfig.Profiles. Empty uses the default ManagerConfig.Rootfs.
	// An unknown profile is rejected with ErrInvalidConfig.
	Profile string

	// RootfsOverride, when set, is an absolute path to a rootfs template
	// to clone for this sandbox instead of a profile or the daemon
	// default — used to boot a converted OCI image from the image store.
	// The daemon resolves it (and sets BootArgs to init the guest
	// agent); Profile and RootfsOverride are mutually exclusive.
	RootfsOverride string

	// StaticNetwork tells the Manager to push the network config to the
	// guest over vsock (netlink) rather than rely on an in-guest DHCP
	// client. Set by the daemon for OCI-image sandboxes.
	StaticNetwork bool

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

	// TokenID attributes this sandbox to the API key that created it, so
	// per-token quotas (scoped-token max_sandboxes) can count it. Empty for
	// unauthenticated (loopback) creates.
	TokenID string

	// Service, when non-nil, is pushed to the guest agent and started
	// after the agent readiness gate. A service that fails to configure
	// or start fails the whole Create (rollback), so a 201 always means
	// the entrypoint is supervised and launched. Requires WaitForAgent
	// (the spec push needs a live agent) and a vsock channel.
	Service *wire.ServiceSpec

	// Publish maps host ports to guest ports (host port publish). Needs
	// a networked sandbox (Network non-nil) so the guest has an IP to
	// forward to, and a configured PortPublisher. A bind failure fails
	// the whole Create.
	Publish []PortMapping

	// DiskBytes, when > 0, grows this sandbox's rootfs clone to at least
	// this size (truncate + resize2fs) after cloning the template and
	// before boot. The shared template is never modified. A no-op when
	// the clone is already at least this large. Requires resize2fs on
	// the host.
	DiskBytes int64
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

	// FullEgress lets the guest reach any public host (metadata/
	// link-local/RFC1918 still blocked). Broadest of the egress modes.
	FullEgress bool

	// CIDRs permits direct egress to IP literals in these public IPv4
	// prefixes, on top of the hostname allowlist. Private/reserved space
	// inside a prefix is still dropped.
	CIDRs []netip.Prefix
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

	// Profiles maps a profile name (e.g. "python-3.12", "node-22") to a
	// pre-baked rootfs image path. A sandbox request naming a profile
	// boots from its image instead of Rootfs; the default Rootfs is used
	// when the request names no profile. Optional — nil disables named
	// profiles (any Profile in a request is then rejected).
	Profiles map[string]string

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

	// PortPublisher, when non-nil, enables host port publishing
	// (CreateConfig.Publish). Nil rejects any create that requests
	// published ports.
	PortPublisher PortPublisher

	// ForkConcurrency caps how many forks Fork boots simultaneously.
	// Each concurrent fork copies a multi-GB rootfs and boots a VM, so
	// an unbounded fan-out on a large count would thrash host I/O and
	// risk an OOM. Zero means DefaultForkConcurrency (runtime.NumCPU()).
	// Fork never spends more than `count` workers regardless of this
	// value.
	ForkConcurrency int

	// MaxForkCount bounds how many sandboxes a single Fork request may
	// create. Fork allocates and spawns proportional to count up front, so
	// this is the guard that stops an unauthenticated ?count=N from OOMing
	// the daemon by fan-out alone. Zero means DefaultMaxForkCount.
	MaxForkCount int

	// StatePath is the file backing the durable sandbox/snapshot
	// registry journal. When non-empty, the
	// Manager records every create/delete/snapshot there so a restart
	// can reconcile (see Manager.Reconcile). Empty disables persistence
	// — the registries live only in memory, as in unit tests.
	StatePath string

	// ReloadAllowlist rebuilds a network allowlist from its persisted
	// patterns when Reconcile re-adopts a snapshot that carried network
	// intent. In production the daemon wires this to network.New; when
	// nil, re-adopted snapshots lose their network policy (networked
	// forks from them would need it re-specified). Optional.
	ReloadAllowlist func(patterns []string) (NetworkAllowlist, error)

	// WakeMinFreeMiB is the wake-admission floor: a wake is refused when host
	// MemAvailable is below this, so waking slept apps can't drive the host into
	// OOM. Zero disables the check (the default for tests/library callers). The
	// woken guest faults its working set in immediately, so this is a "don't wake
	// anything when the host is already starved" guard, not a per-guest reserve.
	WakeMinFreeMiB int

	// MemAvailableMiB reports host memory available for a new workload, in MiB.
	// Nil uses /proc/meminfo (production); tests inject a stub. Consulted only
	// when WakeMinFreeMiB > 0.
	MemAvailableMiB func() (int, error)

	// QuotaPolicy selects how host-side cgroup v2 limits are set on each
	// sandbox's VMM. The zero value (QuotaPolicyOff) applies no limits,
	// so tests and library callers get today's behavior by default; the
	// daemon sets QuotaPolicyDerive. Enforced only under the jailer
	// runner (the direct-exec runner ignores Quotas by design).
	QuotaPolicy QuotaPolicy

	// Metrics, when non-nil, receives operational counters/histograms
	// (sandbox creates, fork and snapshot-restore latencies). Nil is a
	// no-op, so tests and library callers need not wire it.
	Metrics *metrics.Metrics
}

// QuotaPolicy selects the host-side cgroup quota strategy for a Manager.
type QuotaPolicy string

const (
	// QuotaPolicyOff applies no cgroup limits (the zero value).
	QuotaPolicyOff QuotaPolicy = ""
	// QuotaPolicyDerive sizes each VMM's cgroup limits from the
	// sandbox's own vCPU/memory request. See deriveQuotas.
	QuotaPolicyDerive QuotaPolicy = "derive"
)

// cgroup quota derivation tunables.
const (
	// cgroupCPUPeriodUs is the cgroup v2 cpu.max period (microseconds);
	// 100ms is the kernel default. Quota is period × vCPUs.
	cgroupCPUPeriodUs = 100000
	// defaultVMMPidsMax bounds processes in the VMM's HOST cgroup
	// (firecracker/jailer threads) as a cheap fork-bomb guard. It does
	// not bound guest processes, which live in a separate pid namespace.
	defaultVMMPidsMax = 1024
	// minMemHeadroomMiB is the floor for VMM memory overhead added on
	// top of the guest RAM request when sizing memory.max.
	minMemHeadroomMiB = 128
)

// deriveQuotas maps a sandbox's own vCPU/memory request to host-side
// cgroup v2 limits on its Firecracker VMM: CPU capped at the vCPU count,
// memory at guest RAM plus headroom for VMM overhead (and, for lazy-mem
// forks, pages faulted into the VMM's cgroup) so a guest using all its
// RAM under normal load doesn't trip the host OOM-killer, and a fixed
// pids guard on the VMM's host cgroup.
func deriveQuotas(vcpus, memMiB int) runner.Quotas {
	headroom := memMiB / 4
	if headroom < minMemHeadroomMiB {
		headroom = minMemHeadroomMiB
	}
	return runner.Quotas{
		CPUMax:         fmt.Sprintf("%d %d", vcpus*cgroupCPUPeriodUs, cgroupCPUPeriodUs),
		MemoryMaxBytes: int64(memMiB+headroom) << 20,
		PIDsMax:        defaultVMMPidsMax,
	}
}

// quotasFor returns the cgroup limits for a sandbox under the Manager's
// configured policy. QuotaPolicyOff (the default) yields zero Quotas, so
// BuildArgs omits every --cgroup flag.
func (m *Manager) quotasFor(vcpus, memMiB int) runner.Quotas {
	if m.cfg.QuotaPolicy != QuotaPolicyDerive {
		return runner.Quotas{}
	}
	return deriveQuotas(vcpus, memMiB)
}

// NetworkProvisioner is the narrow slice of internal/network.Manager
// that sandbox.Manager depends on. Stated as an interface so the
// two packages stay unordered in the import graph and so tests can
// substitute a trivial stub.
type NetworkProvisioner interface {
	Setup(ctx context.Context, req NetworkSetupRequest) (*NetworkHandle, error)
	Teardown(ctx context.Context, h *NetworkHandle) error
}

// PortMapping is one host→guest port forward — the sandbox-layer view,
// decoupled from sdk/api (same reason NetworkAllowlist is an
// interface here: keep the import graph clean).
type PortMapping struct {
	HostIP    string
	HostPort  int
	GuestPort int
	Protocol  string
}

// PortPublisher publishes host ports to a sandbox's guest and returns a
// handle closed on Delete. The daemon implements it over
// internal/portpublish; nil means publishing is unavailable, so any
// CreateConfig with Publish set is rejected at Create.
type PortPublisher interface {
	Publish(ctx context.Context, sandboxID, guestIP string, ports []PortMapping) (PublishHandle, error)
}

// PublishHandle is an opaque reference to a sandbox's active
// forwarders; Close stops them and drains in-flight connections.
type PublishHandle interface {
	Close()
}

// NetworkSetupRequest is the argument to NetworkProvisioner.Setup.
// Mirrors internal/network.SandboxSetup but lives here so
// sandbox.Manager doesn't import the network package directly.
type NetworkSetupRequest struct {
	SandboxID  string
	Allowlist  NetworkAllowlist
	FullEgress bool
	CIDRs      []netip.Prefix
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

	// HostVeth is the sandbox's host-side veth device in the root netns —
	// where all the guest's traffic flows. Used by host-side packet capture.
	HostVeth string

	// Gateway is the host-side veth IP (guest's default route).
	Gateway string

	// PrefixBits is the guest address's network prefix length (30 for
	// the /30 allocations). DNSServer is the resolver the guest should
	// use (the DNS-proxy anycast). Both feed the static network config
	// the daemon pushes to OCI guests, which can't self-configure.
	PrefixBits int
	DNSServer  string

	// Allowlist is the matcher used at setup time. Retained on
	// the handle because snapshots copy it onto Snapshot.Network
	// so forks can re-register an identical policy without the
	// daemon having to re-parse patterns. Matchers are
	// immutable; sharing a single instance across source + forks
	// is safe.
	Allowlist NetworkAllowlist

	// FullEgress and CIDRs mirror the source sandbox's range-based egress
	// so a fork reconstructs the same policy (carried onto Snapshot.Network
	// alongside Allowlist).
	FullEgress bool
	CIDRs      []netip.Prefix

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

	// StaticNetwork carries the source's boot mode so forks refresh
	// their network the right way — a static netlink push for OCI
	// guests, the DHCP link-bounce for profile guests.
	StaticNetwork bool

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

	// store is the durable registry journal; nil when StatePath is
	// unset (persistence disabled). loadedSandboxes/loadedSnapshots
	// hold the records replayed at construction, consumed once by
	// Reconcile and then cleared.
	store           *store
	loadedSandboxes []sandboxRecord
	loadedSnapshots []snapshotRecord
}

// NewManager constructs a Manager. It starts no goroutines. It touches
// the filesystem only when cfg.StatePath is set: it opens (creating if
// needed) and replays the durable registry journal so a subsequent
// Reconcile call can adopt/reap the previous run's state. The parent of
// StatePath must already exist.
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
	m := &Manager{
		cfg:       cfg,
		sandboxes: make(map[string]*Sandbox),
		snapshots: make(map[string]*Snapshot),
	}
	if cfg.StatePath != "" {
		st, sbx, snaps, err := openStore(cfg.StatePath)
		if err != nil {
			return nil, err
		}
		m.store = st
		m.loadedSandboxes = sbx
		m.loadedSnapshots = snaps
	}
	return m, nil
}

// registerSandbox records a freshly-built sandbox in the in-memory map
// and, when persistence is enabled, appends its record to the durable
// journal. A journal write failure is logged but not fatal: the sandbox
// is live and usable; the worst case is that a crash before the next
// compaction leaves the sandbox unreaped, exactly the pre-gap-3 behavior.
func (m *Manager) registerSandbox(s *Sandbox) {
	m.mu.Lock()
	m.sandboxes[s.ID] = s
	m.mu.Unlock()
	if m.store != nil {
		if err := m.store.putSandbox(sandboxRecordOf(s)); err != nil {
			slog.Default().Warn("persist sandbox record failed", "component", "sandbox", "id", s.ID, "err", err)
		}
	}
}

// Profiles returns the configured rootfs profile names, sorted. Empty
// when the Manager was built without any profiles (no --rootfs-dir).
func (m *Manager) Profiles() []string {
	names := make([]string, 0, len(m.cfg.Profiles))
	for k := range m.cfg.Profiles {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
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
	if vcpus > MaxVCPUs {
		return nil, fmt.Errorf("%w: vcpus %d exceeds max %d", ErrInvalidConfig, vcpus, MaxVCPUs)
	}
	memMiB := req.MemoryMiB
	if memMiB <= 0 {
		memMiB = DefaultMemoryMiB
	}
	if memMiB > MaxMemoryMiB {
		return nil, fmt.Errorf("%w: memory_mib %d exceeds max %d", ErrInvalidConfig, memMiB, MaxMemoryMiB)
	}

	// Resolve the rootfs template: an explicit override (a converted OCI
	// image) wins; otherwise a named profile picks a pre-baked image,
	// else the daemon's default rootfs. Validated up front so a bad
	// selection fails cleanly before any workdir is created.
	rootfsTemplate := m.cfg.Rootfs
	switch {
	case req.RootfsOverride != "":
		rootfsTemplate = req.RootfsOverride
	case req.Profile != "":
		p, ok := m.cfg.Profiles[req.Profile]
		if !ok {
			return nil, fmt.Errorf("%w: unknown profile %q", ErrInvalidConfig, req.Profile)
		}
		rootfsTemplate = p
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
	if err := fsutil.Clone(rootfsTemplate, sbxRootfs); err != nil {
		return nil, fmt.Errorf("sandbox: clone rootfs template: %w", err)
	}

	// Optional per-sandbox disk override: grow only this clone, never the
	// shared template. A no-op when DiskBytes is unset or already covered by
	// the clone's headroom.
	if err := growRootfs(ctx, sbxRootfs, req.DiskBytes); err != nil {
		return nil, err
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
		Quotas:    m.quotasFor(vcpus, memMiB),
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
		ID:            id,
		VCPUs:         vcpus,
		MemoryMiB:     memMiB,
		Workdir:       workdir,
		Profile:       req.Profile,
		TokenID:       req.TokenID,
		StaticNetwork: req.StaticNetwork,
		VSockPath:     handle.VSockPath(),
		CreatedAt:     time.Now().UTC(),
		Network:       netHandle,
		handle:        handle,
		done:          make(chan struct{}),
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

	// Static network push: an OCI-image guest has no DHCP client, so the
	// daemon sends the address it allocated and the agent programs eth0
	// via netlink. Before the service starts (which may need network).
	// Fatal — an unreachable guest is a failed create.
	if req.StaticNetwork && netHandle != nil {
		if s.execClient == nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("%w: static network requires an agent channel (no vsock)", ErrInvalidConfig)
		}
		if err := s.execClient.ConfigureNetwork(ctx, staticNetConfig(s.ID, netHandle)); err != nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("sandbox: configure network: %w", err)
		}
	}

	// Optional supervised service: push the spec and start it before the
	// sandbox is registered, so a successful Create always returns with
	// the entrypoint launched. Failures roll the whole create back —
	// half-created "sandbox up, service broken" states help nobody.
	if req.Service != nil {
		if s.execClient == nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("%w: service requires an agent channel (no vsock)", ErrInvalidConfig)
		}
		if _, err := s.execClient.ConfigureService(ctx, req.Service); err != nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("sandbox: configure service: %w", err)
		}
		if _, err := s.execClient.StartService(ctx); err != nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("sandbox: start service: %w", err)
		}
	}

	// Host port publish: start the forwarders after the service is up so
	// early connections reach a live server. A bind failure (host port
	// in use) is fatal — the whole create rolls back. Last fallible
	// step, so its own rollback (Publish closes any listeners it opened)
	// plus handle.Shutdown here is sufficient.
	if len(req.Publish) > 0 {
		if netHandle == nil || netHandle.GuestIP == "" {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("%w: publish requires a networked sandbox (a guest IP to forward to)", ErrInvalidConfig)
		}
		if m.cfg.PortPublisher == nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("%w: port publishing is not enabled on this daemon", ErrInvalidConfig)
		}
		ph, err := m.cfg.PortPublisher.Publish(ctx, s.ID, netHandle.GuestIP, req.Publish)
		if err != nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("sandbox: publish ports: %w", err)
		}
		s.publish = ph
		s.Published = req.Publish
	}

	m.registerSandbox(s)

	if req.TimeoutSec > 0 {
		m.startLifetimeTimer(s, req.TimeoutSec)
	}

	m.cfg.Metrics.IncSandboxCreated()

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
		SandboxID:  sanitizeNetworkID(sandboxID),
		Allowlist:  req.Allowlist,
		FullEgress: req.FullEgress,
		CIDRs:      req.CIDRs,
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
	req wire.ExecRequest,
	stdout, stderr io.Writer,
) (wire.ExecResult, error) {
	s, err := m.Get(id)
	if err != nil {
		return wire.ExecResult{}, err
	}
	if s.execClient == nil {
		return wire.ExecResult{}, fmt.Errorf("sandbox %s has no agent vsock path", id)
	}
	return s.execClient.Exec(ctx, req, stdout, stderr)
}

// ExecInteractive opens a full-duplex framed exec session to the sandbox's
// guest agent and returns the raw connection (see
// agentapi.Client.ExecInteractive). The caller owns the conn and must Close
// it to end the session; closing it terminates the command on the guest.
//
// Fails fast with ErrNotFound for unknown IDs, or a clear error when the
// sandbox has no agent client (e.g. test stubs).
func (m *Manager) ExecInteractive(ctx context.Context, id string, req wire.ExecRequest) (net.Conn, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if s.execClient == nil {
		return nil, fmt.Errorf("sandbox %s has no agent vsock path", id)
	}
	return s.execClient.ExecInteractive(ctx, req)
}

// PutFiles streams a tar archive to the sandbox's guest agent, which extracts
// it beneath dest (an absolute directory inside the guest). See
// agentapi.Client.PushFiles. Fails fast with ErrNotFound for unknown IDs, or a
// clear error when the sandbox has no agent client.
func (m *Manager) PutFiles(ctx context.Context, id, dest string, tar io.Reader) (wire.FilesPutResult, error) {
	s, err := m.Get(id)
	if err != nil {
		return wire.FilesPutResult{}, err
	}
	if s.execClient == nil {
		return wire.FilesPutResult{}, fmt.Errorf("sandbox %s has no agent vsock path", id)
	}
	return s.execClient.PushFiles(ctx, dest, tar)
}

// ReadFile reads a single file at path inside the sandbox's guest and returns
// its bytes (capped at maxBytes). See agentapi.Client.ReadFile. Fails fast with
// ErrNotFound for unknown IDs.
func (m *Manager) ReadFile(ctx context.Context, id, path string, maxBytes int) ([]byte, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if s.execClient == nil {
		return nil, fmt.Errorf("sandbox %s has no agent vsock path", id)
	}
	return s.execClient.ReadFile(ctx, path, maxBytes)
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

// SnapshotCount returns the number of registered snapshots — fork snapshots
// plus the durable per-instance sleep snapshots. Read as a Prometheus gauge for
// storage/scale-to-zero visibility.
func (m *Manager) SnapshotCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.snapshots)
}

// Routable returns the guest IP to route inbound traffic to, or ("", false)
// when the sandbox is unknown, has no network, or is asleep — a slept sandbox's
// VMM is stopped, so its IP must not be routed to until it is woken. Reads the
// asleep flag under the registry lock (it is written under the same lock by
// SleepInPlace/WakeInPlace), so this is race-free.
func (m *Manager) Routable(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sandboxes[id]
	if !ok || s.asleep != nil || s.Network == nil || s.Network.GuestIP == "" {
		return "", false
	}
	return s.Network.GuestIP, true
}

// HostIface returns the sandbox's host-side veth device (root netns) for a live,
// networked sandbox — the interface a host-side packet capture listens on. False
// if the sandbox is unknown, asleep, or has no network.
func (m *Manager) HostIface(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sandboxes[id]
	if !ok || s.asleep != nil || s.Network == nil || s.Network.HostVeth == "" {
		return "", false
	}
	return s.Network.HostVeth, true
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

// CountByToken returns how many live sandboxes were created by the given API
// key id — the count a scoped token's max_sandboxes is enforced against. A
// tokenID of "" (unauthenticated) is never counted here; those creates aren't
// policy-bounded.
func (m *Manager) CountByToken(tokenID string) int {
	if tokenID == "" {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, s := range m.sandboxes {
		if s.TokenID == tokenID {
			n++
		}
	}
	return n
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
		// A slept sandbox has no live VMM to drain, and its durable snapshot must
		// survive so the app is re-adopted (and woken from it) on the next start.
		// Delete would GC that snapshot — so leave slept sandboxes alone; their
		// orphan workdir/netns are cleaned by the startup reaps, snapshot kept.
		m.mu.RLock()
		asleep := s.asleep != nil
		m.mu.RUnlock()
		if asleep {
			continue
		}
		_ = m.Delete(ctx, s.ID)
	}
	if m.store != nil {
		_ = m.store.close()
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

	// Bound the operation to guest memory. r.Context() carries no deadline
	// and CreateSnapshot (which writes the whole memory file) has no fcapi
	// wall-clock cap, so without this a wedged firecracker would hang this
	// goroutine forever. A large guest still gets ample headroom.
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout(src.MemoryMiB))
	defer cancel()

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
	// From here on, any failure must attempt to resume the source. The
	// operation ctx is unusable for the rollback — it may be why we failed,
	// and is cancelled once the sized budget expires — so resume on a fresh,
	// bounded ctx so a wedged firecracker can't hang this goroutine forever.
	defer func() {
		if !success {
			rbCtx, rbCancel := context.WithTimeout(context.Background(), resumeRollbackTimeout)
			defer rbCancel()
			_ = src.handle.Resume(rbCtx)
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
		ID:            snapID,
		SourceID:      src.ID,
		VCPUs:         src.VCPUs,
		MemoryMiB:     src.MemoryMiB,
		Dir:           snapDir,
		StatePath:     snapState,
		MemPath:       snapMem,
		RootfsPath:    snapRootfs,
		StaticNetwork: src.StaticNetwork,
		CreatedAt:     time.Now().UTC(),
	}
	// Record the source's network intent so forks reconstruct a
	// matching config. Matchers are immutable, so sharing the
	// same instance across source + forks is safe — simpler than
	// re-parsing patterns for every fork.
	if src.Network != nil && src.Network.Allowlist != nil {
		snap.Network = &NetworkConfig{
			Allowlist:  src.Network.Allowlist,
			FullEgress: src.Network.FullEgress,
			CIDRs:      src.Network.CIDRs,
		}
	}

	m.mu.Lock()
	m.snapshots[snapID] = snap
	m.mu.Unlock()

	if m.store != nil {
		if err := m.store.putSnapshot(snapshotRecordOf(snap)); err != nil {
			slog.Default().Warn("persist snapshot record failed", "component", "sandbox", "id", snapID, "err", err)
		}
	}

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
	// Checked before touching the registry: once the entry is removed the
	// on-disk delete must run to completion, or the files leak with no
	// record pointing at them.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sandbox: delete snapshot %s: %w", id, err)
	}
	m.mu.Lock()
	snap, ok := m.snapshots[id]
	if !ok {
		m.mu.Unlock()
		return ErrSnapshotNotFound
	}
	delete(m.snapshots, id)
	m.mu.Unlock()

	if m.store != nil {
		if err := m.store.delSnapshot(id); err != nil {
			slog.Default().Warn("persist snapshot deletion failed", "component", "sandbox", "id", id, "err", err)
		}
	}

	if err := os.RemoveAll(snap.Dir); err != nil {
		return fmt.Errorf("sandbox: remove snapshot dir %s: %w", snap.Dir, err)
	}
	return nil
}

// defaultForkConcurrency is the fork fan-out cap used when
// ManagerConfig.ForkConcurrency is left at 0. NumCPU is a reasonable
// proxy for how much parallel rootfs-copy + VM-boot work a host can
// absorb without thrashing.
func defaultForkConcurrency() int {
	if n := runtime.NumCPU(); n > 0 {
		return n
	}
	return 1
}

// MaxForkCount reports the effective per-request fork ceiling, honoring
// ManagerConfig.MaxForkCount and falling back to DefaultMaxForkCount.
// Exposed so the HTTP layer can reject an oversized ?count before calling
// Fork.
func (m *Manager) MaxForkCount() int {
	if m.cfg.MaxForkCount > 0 {
		return m.cfg.MaxForkCount
	}
	return DefaultMaxForkCount
}

// Fork creates `count` new sandboxes from a snapshot. Each fork gets
// its own workdir, per-fork memory file (cloned from the snapshot's),
// and per-fork rootfs (cloned from the snapshot's frozen rootfs).
//
// Forks run in parallel, but the fan-out is bounded to at most
// ForkConcurrency workers (default runtime.NumCPU()) so a large count
// can't launch hundreds of simultaneous rootfs copies and VM boots and
// OOM the host. The dominant per-fork cost on a filesystem without
// reflink support is byte-copying the memory file (~0.5 GB) and rootfs
// (~1 GB), which is I/O-bound and parallelizes well; the non-I/O
// portions (jailer staging, LoadSnapshot, vCPU restore) are
// independent. On a test host we measured ~2.1s/fork serially vs
// ~1.0s/fork in parallel (count=3).
//
// If any fork in the batch fails to start, every fork that
// succeeded is torn down before returning — Fork remains
// all-or-nothing. The first observed error is returned (indexed by
// its fork position for readability).
func (m *Manager) Fork(ctx context.Context, snapshotID string, count int, tokenID string, publish []PortMapping) ([]*Sandbox, error) {
	if count <= 0 {
		return nil, errors.New("sandbox: Fork count must be > 0")
	}
	// Host ports are exclusive, so a fan-out cannot share a publish
	// mapping — publishing is only meaningful for a single fork.
	if len(publish) > 0 && count != 1 {
		return nil, fmt.Errorf("%w: publish requires count 1 (host ports are exclusive, got count %d)", ErrInvalidConfig, count)
	}
	// Reject an oversized count *before* allocating results/goroutines
	// proportional to it — the concurrency semaphore below bounds only how
	// many forkOne bodies run at once, not the up-front fan-out, so an
	// unclamped count would OOM the daemon before it throttled anything.
	if max := m.MaxForkCount(); count > max {
		return nil, fmt.Errorf("%w: fork count %d exceeds max %d", ErrInvalidConfig, count, max)
	}
	snap, err := m.GetSnapshot(snapshotID)
	if err != nil {
		return nil, err
	}

	type forkResult struct {
		sb  *Sandbox
		err error
	}

	// Bound the fan-out: a buffered channel of `limit` tokens caps how
	// many forkOne calls (each a rootfs copy + VM boot) run at once.
	limit := m.cfg.ForkConcurrency
	if limit <= 0 {
		limit = defaultForkConcurrency()
	}
	if limit > count {
		limit = count
	}
	sem := make(chan struct{}, limit)

	results := make([]forkResult, count)
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sb, err := m.forkOne(ctx, snap, tokenID, publish, false)
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
func (m *Manager) forkOne(ctx context.Context, snap *Snapshot, tokenID string, publish []PortMapping, wake bool) (*Sandbox, error) {
	// Bound the restore to guest memory, for the same reason Snapshot does:
	// LoadSnapshot carries no fcapi wall-clock cap and r.Context() no
	// deadline, so a wedged firecracker must not hang this goroutine. Forks
	// restore with LazyMem (pages served on demand), so this is headroom, not
	// a tight budget.
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout(snap.MemoryMiB))
	defer cancel()

	forkStart := time.Now()

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

	if err := fsutil.Clone(snap.RootfsPath, forkRootfs); err != nil {
		return nil, fmt.Errorf("clone snapshot rootfs: %w", err)
	}
	// Guest memory is deliberately NOT cloned. The fork restores with
	// LazyMem: the runner's memfault handler serves pages on demand
	// straight from the snapshot's memory file, shared read-only by
	// every fork, and UFFDIO_COPY installs them into this fork's
	// private memory — so writes diverge per fork with no up-front
	// copy. This keeps fork cost O(guest working set) on any
	// filesystem, reflink or not.

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
		MemPath:    snap.MemPath,
		RootfsPath: forkRootfs,
		LazyMem:    true,
		Quotas:     m.quotasFor(snap.VCPUs, snap.MemoryMiB),
	}
	if netHandle != nil {
		restoreSpec.NetNS = netHandle.NetnsPath
	}

	restoreStart := time.Now()
	handle, err := m.cfg.Runner.Restore(ctx, restoreSpec)
	if err != nil {
		return nil, fmt.Errorf("runner restore: %w", err)
	}
	m.cfg.Metrics.ObserveSnapshotRestore(time.Since(restoreStart))

	s := &Sandbox{
		ID:               id,
		VCPUs:            snap.VCPUs,
		MemoryMiB:        snap.MemoryMiB,
		Workdir:          workdir,
		TokenID:          tokenID,
		SourceSnapshotID: snap.ID,
		StaticNetwork:    snap.StaticNetwork,
		VSockPath:        handle.VSockPath(),
		CreatedAt:        time.Now().UTC(),
		Network:          netHandle,
		handle:           handle,
		done:             make(chan struct{}),
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

	// Clone-safety: before the fork is
	// registered — and therefore before anything can exec into it —
	// hand the guest a fresh 32-byte seed and have the agent reseed
	// the kernel CRNG and rotate machine-id + hostname. Fatal on
	// failure, unlike the network refresh below: duplicated entropy
	// doesn't self-heal, and a fork without the uniqueness guarantee
	// is worse than no fork.
	//
	// A fork with no agent channel can't be refreshed at all, so it must
	// fail rather than register un-refreshed. Unreachable today (jailer
	// restores always set VSockPath) but fatal by design intent.
	if s.execClient == nil {
		_ = handle.Shutdown(context.Background())
		return nil, fmt.Errorf("fork %s: no agent channel for clone-safety refresh", id)
	}
	// Fork rotates identity (fresh machine-id/hostname) + reseeds RNG. Wake (a
	// restart-recovery restore of a slept app) instead PRESERVES identity —
	// reseed RNG + step the clock only — matching in-place wake's contract.
	if wake {
		if err := m.wakeRefresh(ctx, s.execClient); err != nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("wake refresh: %w", err)
		}
	} else if err := m.refreshIdentity(ctx, s.execClient, id); err != nil {
		_ = handle.Shutdown(context.Background())
		return nil, fmt.Errorf("identity refresh: %w", err)
	}

	// Re-apply the fork's network onto its new per-netns /30. The
	// restored guest holds the source's eth0 config from snapshot time.
	// OCI guests (StaticNetwork) get a fresh netlink push — they have no
	// DHCP client to self-heal, so this is fatal. Profile guests get the
	// DHCP link-bounce, which is non-fatal (they recover on the next
	// renewal cycle).
	if netHandle != nil && s.execClient != nil {
		refreshCtx, cancel := context.WithTimeout(ctx, networkRefreshTimeout)
		if s.StaticNetwork {
			if err := s.execClient.ConfigureNetwork(refreshCtx, staticNetConfig(s.ID, netHandle)); err != nil {
				cancel()
				_ = handle.Shutdown(context.Background())
				return nil, fmt.Errorf("fork %s: configure network: %w", id, err)
			}
		} else if err := s.execClient.RefreshNetwork(refreshCtx); err != nil {
			// Log-and-continue: the guest recovers on its own within one
			// DHCP renewal cycle (~30s given our 60s lease).
			_ = err
		}
		cancel()
	}

	// Host port publish, mirroring Create: forwarders start only once the
	// fork's guest is reachable on its fresh /30, and a bind failure rolls
	// the fork back (Publish closes any listeners it opened).
	if len(publish) > 0 {
		if netHandle == nil || netHandle.GuestIP == "" {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("%w: publish requires a networked sandbox (a guest IP to forward to)", ErrInvalidConfig)
		}
		if m.cfg.PortPublisher == nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("%w: port publishing is not enabled on this daemon", ErrInvalidConfig)
		}
		ph, err := m.cfg.PortPublisher.Publish(ctx, s.ID, netHandle.GuestIP, publish)
		if err != nil {
			_ = handle.Shutdown(context.Background())
			return nil, fmt.Errorf("fork %s: publish ports: %w", id, err)
		}
		s.publish = ph
		s.Published = publish
	}

	// A wake-from-snapshot is the sole consumer of its (sleep) snapshot, whose
	// memory file backs this instance's lazy memory. Own it so the next sleep or
	// Delete GCs it. Regular forks share the snapshot and never own it.
	if wake {
		s.memSnapshotID = snap.ID
	}

	m.registerSandbox(s)

	m.cfg.Metrics.IncSandboxCreated()
	m.cfg.Metrics.ObserveForkDuration(time.Since(forkStart))

	success = true
	return s, nil
}

// WakeFromSnapshot restores a durable sleep snapshot into a FRESH instance with
// wake semantics (reseed RNG + step clock, identity preserved) and a fresh
// network. Used to wake a slept app after a daemon restart, when its original
// in-place instance is gone; the returned sandbox is the app's new instance.
func (m *Manager) WakeFromSnapshot(ctx context.Context, snapshotID string, publish []PortMapping) (*Sandbox, error) {
	snap, err := m.GetSnapshot(snapshotID)
	if err != nil {
		return nil, err
	}
	if err := m.admitWake(); err != nil {
		return nil, err
	}
	return m.forkOne(ctx, snap, "", publish, true)
}

// staticNetConfig builds the network config the daemon pushes to an
// OCI guest (which has no DHCP client) from the allocated handle. Used
// at create and re-used on fork with the fork's fresh handle.
func staticNetConfig(sandboxID string, h *NetworkHandle) agentwire.NetworkConfigRequest {
	req := agentwire.NetworkConfigRequest{
		Address:   h.GuestIP,
		PrefixLen: h.PrefixBits,
		Gateway:   h.Gateway,
		Hostname:  sandboxID,
	}
	if h.DNSServer != "" {
		req.DNS = []string{h.DNSServer}
	}
	return req
}

// networkRefreshTimeout bounds the post-resume RefreshNetwork RPC.
// Must be at least as long as the agent's own internal timeout
// (10s for the down→up→wait dance) plus slack for the vsock
// round-trip.
const networkRefreshTimeout = 15 * time.Second

// identityRefreshTimeout bounds the post-resume identity refresh,
// retries included. Generous relative to the work (two ioctls plus a
// few file writes) because the window has to absorb agent wake-up
// when WaitForAgent is disabled.
const identityRefreshTimeout = 15 * time.Second

// identityRefreshPollInterval is the retry cadence within
// identityRefreshTimeout, mirroring agentReadyPollInterval.
const identityRefreshPollInterval = 200 * time.Millisecond

// identitySeedSize is the per-fork entropy payload: 32 bytes
// (256 bits), a full reseed's worth for the guest kernel CRNG.
const identitySeedSize = 32

// refreshIdentity drives the guest agent's POST /identity/refresh for
// a just-restored fork with a fresh host-generated seed — the
// clone-safety guarantee that no two forks wake with the same RNG
// state, machine-id, or hostname. It retries on the agent-readiness
// cadence because the agent may still be waking and the refresh must
// not depend on the optional WaitForAgent setting. A stale-rootfs 404
// aborts immediately — retrying cannot fix an agent that lacks the
// endpoint.
func (m *Manager) refreshIdentity(ctx context.Context, c *agentapi.Client, sandboxID string) error {
	seed := make([]byte, identitySeedSize)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("generate seed: %w", err)
	}

	refreshCtx, cancel := context.WithTimeout(ctx, identityRefreshTimeout)
	defer cancel()

	var lastErr error
	for {
		lastErr = c.RefreshIdentity(refreshCtx, seed, sandboxID)
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, agentapi.ErrIdentityRefreshUnsupported) {
			return lastErr
		}
		select {
		case <-refreshCtx.Done():
			return fmt.Errorf("%w (last attempt: %v)", refreshCtx.Err(), lastErr)
		case <-time.After(identityRefreshPollInterval):
		}
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
	memSnap := s.memSnapshotID // durable sleep snapshot backing this instance, if any
	m.mu.Unlock()

	// Durably drop the record so a crash after this point doesn't leave
	// the (now torn-down) sandbox to be reaped again on restart.
	if m.store != nil {
		if err := m.store.delSandbox(id); err != nil {
			slog.Default().Warn("persist sandbox deletion failed", "component", "sandbox", "id", id, "err", err)
		}
	}

	// Signal any lifetime-timer goroutine to exit before we block on
	// shutdown. Safe to call more than once; doneOnce guards the close.
	s.doneOnce.Do(func() { close(s.done) })

	// Stop the host port forwarders first: close the listeners (freeing
	// the host ports) and drain in-flight connections before the VM goes
	// away. In-memory, so no persistence to reconcile.
	if s.publish != nil {
		s.publish.Close()
	}

	shutdownErr := s.handle.Shutdown(ctx)

	// The VMM (and, for a woken instance, its uffd pager) is now stopped, so the
	// durable sleep snapshot backing its memory is free to reclaim. Separate dir
	// under WorkBase, so the workdir removal below wouldn't reach it.
	if memSnap != "" {
		_ = m.DeleteSnapshot(context.Background(), memSnap)
	}

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
