package network

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/network/dhcp"
	"github.com/gnana997/crucible/internal/network/dnsproxy"
)

// DefaultSubnetPool is the base CIDR we allocate /30 blocks from
// when the operator doesn't override it. 10.20.0.0/16 = 16 384
// /30 slots, far more concurrent sandboxes than a single host can
// run.
var DefaultSubnetPool = netip.MustParsePrefix("10.20.0.0/16")

// DefaultDNSAnycast is the reserved address we bind the shared
// DNS proxy to. Lives inside DefaultSubnetPool so the reservation
// is automatic; we excise the containing /30 from the pool at
// construction time so no sandbox gets allocated over it.
var DefaultDNSAnycast = netip.MustParseAddr("10.20.255.254")

// crucibleDNSIface is the dummy interface we create in host root
// netns to carry the DNS anycast address. Name is fixed so orphan
// reap can find it across daemon restarts.
const crucibleDNSIface = "crucible-dns"

// ManagerConfig configures the daemon-wide network.Manager.
type ManagerConfig struct {
	// SubnetPool is the base CIDR; /30s are allocated from it
	// per sandbox. Must be at least /28 in practice (four /30s)
	// to leave room for the DNS anycast reservation plus a real
	// sandbox.
	SubnetPool netip.Prefix

	// DNSAnycast is the host-side address where every sandbox
	// sends DNS. Must be inside SubnetPool. The block containing
	// it is reserved out of the allocator.
	DNSAnycast netip.Addr

	// EgressIface is the host interface nftables masquerades
	// outbound traffic on (e.g., "eth0", "wlan0"). Operators
	// set this to whichever interface reaches the public
	// network.
	EgressIface string

	// DNSUpstream is a value accepted by dnsproxy.ResolveUpstream:
	// "" or "system" → reads /etc/resolv.conf; "ip" → ip:53;
	// "ip:port" → exact.
	DNSUpstream string

	// Logger receives Manager lifecycle events. Nil means
	// slog.Default.
	Logger *slog.Logger
}

// Manager owns the daemon-wide network state. One per daemon.
//
// Lifecycle:
//
//   - Start does the one-time host setup (dummy iface, nft table,
//     DNS proxy).
//   - Setup / Teardown handle per-sandbox state.
//   - Stop reverses Start.
//
// Per-sandbox Setup and Teardown are safe to call concurrently;
// the internal Pool + DNS proxy policies are mutex-protected.
type Manager struct {
	cfg   ManagerConfig
	pool  *Pool
	proxy *dnsproxy.Proxy
	log   *slog.Logger

	mu       sync.Mutex
	started  bool
	sandboxes map[string]*SandboxHandle // sanitized ID → handle
}

// SandboxHandle is the return value of Setup. Keep it for the
// sandbox's lifetime and pass to Teardown on cleanup.
//
// Fields are read-only from the caller's perspective.
type SandboxHandle struct {
	// SandboxID is the sanitized ID used in all interface /
	// netns / nft-chain names derived from it.
	SandboxID string

	// Netns is the netns name (without the /var/run/netns/
	// prefix). Pass this to jailer's --netns flag.
	Netns string

	// NetnsPath is the absolute host path to the netns pseudo-
	// file. This is what unix.Setns and jailer's --netns both
	// want.
	NetnsPath string

	// Lease is the /30 allocated for this sandbox.
	Lease Lease

	// GuestMAC is the deterministic MAC we hand to Firecracker's
	// PutNetworkInterface and the in-netns DHCP responder.
	GuestMAC [6]byte

	// TapName is the interface Firecracker attaches to. Fixed
	// (see veth.go); exposed here so the runner caller doesn't
	// need to know the package constant.
	TapName string

	// internal
	dhcp *dhcp.Responder
}

// SandboxSetup is the input to Manager.Setup.
type SandboxSetup struct {
	// SandboxID must be sanitized (alphanumeric + hyphens, 1..64).
	// The caller typically passes the jailer-safe form of the
	// sandbox ID (underscores → hyphens).
	SandboxID string

	// Allowlist governs which hostnames the sandbox's guest can
	// resolve. Must be non-nil; pass a zero Allowlist (constructed
	// from an empty slice) if the caller wants to explicitly ban
	// everything but still plumb DNS (a rare case).
	Allowlist *Allowlist
}

// Start does the one-time host setup and returns a Manager.
// Requires root (or CAP_NET_ADMIN + CAP_SYS_ADMIN). Fails fast on
// any step so callers can refuse to serve requests if the network
// layer didn't come up cleanly.
func Start(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if cfg.SubnetPool == (netip.Prefix{}) {
		cfg.SubnetPool = DefaultSubnetPool
	}
	if cfg.DNSAnycast == (netip.Addr{}) {
		cfg.DNSAnycast = DefaultDNSAnycast
	}
	if cfg.EgressIface == "" {
		return nil, errors.New("network: EgressIface required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "network")

	// 1. Pool — with the DNS anycast's /30 reserved.
	pool, err := NewPool(cfg.SubnetPool, cfg.DNSAnycast)
	if err != nil {
		return nil, fmt.Errorf("network: pool: %w", err)
	}

	// 2. Dummy interface for the DNS anycast.
	if err := ensureCrucibleDNSIface(ctx, cfg.DNSAnycast); err != nil {
		return nil, err
	}

	// 3. nftables base table.
	if err := EnsureBaseTable(ctx, cfg.EgressIface); err != nil {
		_ = removeCrucibleDNSIface(context.Background())
		return nil, fmt.Errorf("network: nft base table: %w", err)
	}

	// 4. DNS proxy.
	upstream, warn := dnsproxy.ResolveUpstream(cfg.DNSUpstream)
	if warn != nil {
		log.Warn("dns upstream fell back", "err", warn)
	}
	listen := net.JoinHostPort(cfg.DNSAnycast.String(), "53")
	proxy, err := dnsproxy.Start(dnsproxy.Config{
		ListenAddr: listen,
		Upstream:   upstream,
		AllowIP:    AllowIP, // package-level helper from nft.go
		Logger:     log,
	})
	if err != nil {
		_ = TeardownBaseTable(context.Background())
		_ = removeCrucibleDNSIface(context.Background())
		return nil, fmt.Errorf("network: dns proxy: %w", err)
	}

	m := &Manager{
		cfg:       cfg,
		pool:      pool,
		proxy:     proxy,
		log:       log,
		started:   true,
		sandboxes: make(map[string]*SandboxHandle),
	}
	log.Info("network started",
		"subnet_pool", cfg.SubnetPool,
		"dns_anycast", cfg.DNSAnycast,
		"dns_upstream", upstream,
		"egress_iface", cfg.EgressIface,
	)
	return m, nil
}

// Stop tears down everything Start created. Best-effort; logs
// per-step errors but always attempts all steps so we don't leak
// host-side state.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = false
	live := make([]*SandboxHandle, 0, len(m.sandboxes))
	for _, h := range m.sandboxes {
		live = append(live, h)
	}
	m.mu.Unlock()

	// Tear down any sandboxes still registered.
	for _, h := range live {
		if err := m.Teardown(ctx, h); err != nil {
			m.log.Warn("teardown during Stop", "sandbox", h.SandboxID, "err", err)
		}
	}

	// DNS proxy.
	if err := m.proxy.Stop(); err != nil {
		m.log.Warn("dns proxy stop", "err", err)
	}
	// nft.
	if err := TeardownBaseTable(ctx); err != nil {
		m.log.Warn("nft teardown", "err", err)
	}
	// Dummy iface.
	if err := removeCrucibleDNSIface(ctx); err != nil {
		m.log.Warn("dummy iface remove", "err", err)
	}
	m.log.Info("network stopped")
	return nil
}

// Setup stands up per-sandbox network state and returns a
// SandboxHandle the caller keeps for the sandbox's lifetime.
//
// Rolls back partial state on any error so a failed Setup leaves
// nothing behind.
func (m *Manager) Setup(ctx context.Context, req SandboxSetup) (*SandboxHandle, error) {
	if req.SandboxID == "" {
		return nil, errors.New("network: Setup: SandboxID required")
	}
	if req.Allowlist == nil {
		return nil, errors.New("network: Setup: Allowlist required")
	}

	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil, errors.New("network: Manager not started")
	}
	m.mu.Unlock()

	// 1. Acquire subnet.
	lease, err := m.pool.Acquire()
	if err != nil {
		return nil, fmt.Errorf("network: acquire subnet: %w", err)
	}
	var success bool
	defer func() {
		if !success {
			m.pool.Release(lease)
		}
	}()

	// 2. Create netns.
	nsName := NetnsPrefix + req.SandboxID
	if err := CreateNetns(ctx, nsName); err != nil {
		return nil, fmt.Errorf("network: create netns: %w", err)
	}
	defer func() {
		if !success {
			_ = DeleteNetns(context.Background(), nsName)
		}
	}()

	// 3. veth + bridge + tap.
	vspec := VethSpec{
		SandboxID:  req.SandboxID,
		Netns:      nsName,
		Lease:      lease,
		DNSAnycast: m.cfg.DNSAnycast,
	}
	if err := Setup(ctx, vspec); err != nil {
		return nil, fmt.Errorf("network: veth setup: %w", err)
	}
	defer func() {
		if !success {
			_ = Teardown(context.Background(), vspec)
		}
	}()

	// 4. nft per-sandbox rules.
	if err := InstallSandbox(ctx, req.SandboxID, vspec.HostVeth(), m.cfg.DNSAnycast); err != nil {
		return nil, fmt.Errorf("network: nft install: %w", err)
	}
	defer func() {
		if !success {
			_ = RemoveSandbox(context.Background(), req.SandboxID, vspec.HostVeth())
		}
	}()

	// 5. DHCP responder.
	mac := macFromSandboxID(req.SandboxID)
	mask, err := dhcp.MaskFromPrefixBits(lease.Prefix.Bits())
	if err != nil {
		return nil, fmt.Errorf("network: subnet mask: %w", err)
	}
	resp, err := dhcp.Start(dhcp.Config{
		Netns:        nsName,
		BindDevice:   vspec.BridgeName(),
		ClientMAC:    mac,
		OfferedIP:    lease.GuestIP,
		Gateway:      lease.Gateway,
		DNS:          m.cfg.DNSAnycast,
		SubnetMask:   mask,
		LeaseSeconds: 60,
		Logger:       m.log,
	})
	if err != nil {
		return nil, fmt.Errorf("network: start dhcp: %w", err)
	}
	defer func() {
		if !success {
			_ = resp.Stop()
		}
	}()

	// 6. Register with DNS proxy so queries from this guest IP
	// are matched against its allowlist.
	m.proxy.Register(lease.GuestIP, &dnsproxy.Policy{
		SandboxID: req.SandboxID,
		Allowlist: req.Allowlist,
	})

	h := &SandboxHandle{
		SandboxID: req.SandboxID,
		Netns:     nsName,
		NetnsPath: NetnsPath(nsName),
		Lease:     lease,
		GuestMAC:  mac,
		TapName:   TapName,
		dhcp:      resp,
	}
	m.mu.Lock()
	m.sandboxes[req.SandboxID] = h
	m.mu.Unlock()
	m.log.Info("sandbox network up",
		"sandbox", req.SandboxID,
		"guest_ip", lease.GuestIP,
		"gateway", lease.Gateway,
	)

	success = true
	return h, nil
}

// Teardown reverses Setup. Each step is best-effort and logged on
// failure — we always attempt all steps so partial failures don't
// leak state across daemon restarts.
//
// Idempotent: calling Teardown on an already-torn-down handle is
// a no-op returning nil.
func (m *Manager) Teardown(ctx context.Context, h *SandboxHandle) error {
	if h == nil {
		return nil
	}

	m.mu.Lock()
	if _, exists := m.sandboxes[h.SandboxID]; !exists {
		m.mu.Unlock()
		return nil // already torn down or never installed
	}
	delete(m.sandboxes, h.SandboxID)
	m.mu.Unlock()

	// Reverse order of Setup.
	m.proxy.Deregister(h.Lease.GuestIP)

	if h.dhcp != nil {
		if err := h.dhcp.Stop(); err != nil {
			m.log.Warn("dhcp stop", "sandbox", h.SandboxID, "err", err)
		}
	}

	vspec := VethSpec{
		SandboxID:  h.SandboxID,
		Netns:      h.Netns,
		Lease:      h.Lease,
		DNSAnycast: m.cfg.DNSAnycast,
	}
	if err := RemoveSandbox(ctx, h.SandboxID, vspec.HostVeth()); err != nil {
		m.log.Warn("nft remove", "sandbox", h.SandboxID, "err", err)
	}
	if err := Teardown(ctx, vspec); err != nil {
		m.log.Warn("veth teardown", "sandbox", h.SandboxID, "err", err)
	}
	if err := DeleteNetns(ctx, h.Netns); err != nil {
		m.log.Warn("netns delete", "sandbox", h.SandboxID, "err", err)
	}

	m.pool.Release(h.Lease)
	m.log.Info("sandbox network down", "sandbox", h.SandboxID)
	return nil
}

// --- helpers ----------------------------------------------------

// macFromSandboxID derives a deterministic, locally-administered
// MAC from the sandbox ID. Locally-administered = bit 1 of the
// first octet is set (we use 0x02); unicast = bit 0 is clear.
//
// Determinism matters because the DHCP responder pre-computes its
// answer for exactly this MAC; both ends must agree.
func macFromSandboxID(id string) [6]byte {
	h := sha1.Sum([]byte(id))
	return [6]byte{0x02, h[0], h[1], h[2], h[3], h[4]}
}

// ensureCrucibleDNSIface creates the dummy interface that carries
// the DNS anycast address in host root netns, idempotently. The
// interface is named crucibleDNSIface so orphan reap can find it
// across daemon restarts.
func ensureCrucibleDNSIface(ctx context.Context, addr netip.Addr) error {
	// Remove any stale version first — easier than conditionally
	// reconfiguring. The interface is ours by name; removing a
	// lingering one from a previous daemon crash is the right
	// thing.
	_ = removeCrucibleDNSIface(ctx)

	if err := runCmd(ctx, "ip", "link", "add", crucibleDNSIface, "type", "dummy"); err != nil {
		return fmt.Errorf("network: create %s: %w", crucibleDNSIface, err)
	}
	cidr := fmt.Sprintf("%s/32", addr)
	if err := runCmd(ctx, "ip", "addr", "add", cidr, "dev", crucibleDNSIface); err != nil {
		_ = runCmd(context.Background(), "ip", "link", "delete", crucibleDNSIface)
		return fmt.Errorf("network: assign %s to %s: %w", cidr, crucibleDNSIface, err)
	}
	if err := runCmd(ctx, "ip", "link", "set", crucibleDNSIface, "up"); err != nil {
		_ = runCmd(context.Background(), "ip", "link", "delete", crucibleDNSIface)
		return fmt.Errorf("network: bring up %s: %w", crucibleDNSIface, err)
	}
	return nil
}

// removeCrucibleDNSIface deletes the dummy interface. Missing-
// iface is treated as success (idempotent).
func removeCrucibleDNSIface(ctx context.Context) error {
	err := runCmd(ctx, "ip", "link", "delete", crucibleDNSIface)
	if err == nil {
		return nil
	}
	if isCannotFindDevice(err) {
		return nil
	}
	return err
}

// WaitForDNSProxyReady blocks until the shared DNS proxy is
// responsive, bounded by a short timeout. Useful in tests and
// in integration smokes where the caller wants to be sure the
// proxy is live before registering a sandbox.
//
// In production, Start only returns after the proxy's
// NotifyStartedFunc has fired, so this is mostly a belt-and-
// suspenders check.
func (m *Manager) WaitForDNSProxyReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.proxy != nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("network: DNS proxy did not become ready")
}
