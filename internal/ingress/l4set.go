package ingress

import (
	"log/slog"
	"net/netip"
	"sort"
	"sync"

	"github.com/gnana997/crucible/sdk/api"
)

// VIPPool hands out per-app internal VIPs. Satisfied by *network.VIPPool; an
// interface here so this package needn't import internal/network.
type VIPPool interface {
	Acquire() (netip.Addr, error)
	Release(addr netip.Addr)
}

// InternalL4Hooks are the side effects of (un)exposing an app's per-app VIP, injected
// so InternalL4Set is testable without root/nft/a live proxy. The daemon wires them to
// network.AddInternalVIP / network.AddL4Ports / Proxy.AddInternalApp (and the removes).
// AddVIP assigns the VIP to the anycast dummy iface; OpenPorts opens the nft accepts;
// Bind starts the proxy's VIP:port listeners. Removal is the reverse order.
type InternalL4Hooks struct {
	AddVIP     func(vip netip.Addr) error
	RemoveVIP  func(vip netip.Addr) error
	OpenPorts  func(vip netip.Addr, tcpPorts []int) error
	ClosePorts func(vip netip.Addr, tcpPorts []int) error
	Bind       func(app string, vip netip.Addr, ports []L4Port) error
	Unbind     func(app string)
}

// InternalL4Set keeps per-app VIP exposure in sync with the apps that declare
// InternalPorts, mirroring WakeForwarderSet for published ports. It owns the VIP pool
// and the app→VIP map; Reconcile diffs the desired set against current state and
// drives the hooks. VIPs are in-memory (no persistence): a peer always reaches an app
// via <app>.internal, which re-resolves through DNS each connection, so a VIP that
// changes across a daemon restart is transparent.
type InternalL4Set struct {
	pool  VIPPool
	hooks InternalL4Hooks
	log   *slog.Logger

	mu    sync.Mutex
	state map[string]*binding // app name → its current binding
}

type binding struct {
	vip   netip.Addr
	ports []L4Port // sorted by port for stable diffing
}

// NewInternalL4Set builds a reconciler over the VIP pool and side-effect hooks.
func NewInternalL4Set(pool VIPPool, hooks InternalL4Hooks, log *slog.Logger) *InternalL4Set {
	if log == nil {
		log = slog.Default()
	}
	return &InternalL4Set{pool: pool, hooks: hooks, log: log, state: map[string]*binding{}}
}

// VIPFor returns an app's assigned VIP (for the DNS answer + AppResponse.InternalVIP).
func (s *InternalL4Set) VIPFor(app string) (netip.Addr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.state[app]
	if !ok {
		return netip.Addr{}, false
	}
	return b.vip, true
}

// VIPForString is VIPFor as a string ("" when unassigned) — the app Manager's
// InternalVIP lookup for toResponse.
func (s *InternalL4Set) VIPForString(app string) string {
	if vip, ok := s.VIPFor(app); ok {
		return vip.String()
	}
	return ""
}

// Reconcile brings VIP exposure in line with specs: apps declaring InternalPorts get a
// VIP + listeners; apps that dropped their ports (or vanished) are torn down. Called at
// the end of each app-reconcile pass with the current desired-running specs. Diff-based,
// so an unchanged set is a cheap no-op. Best-effort: a failing hook is logged, not fatal.
func (s *InternalL4Set) Reconcile(specs []api.AppSpec) {
	desired := make(map[string][]L4Port, len(specs))
	for _, sp := range specs {
		if len(sp.InternalPorts) == 0 {
			continue
		}
		desired[sp.Name] = sortedL4Ports(sp.InternalPorts)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove apps no longer desired, or whose ports changed (torn down then re-added).
	for app, b := range s.state {
		want, ok := desired[app]
		if ok && samePorts(b.ports, want) {
			continue // unchanged
		}
		s.teardownLocked(app, b)
	}

	// Add newly-desired apps (and re-add the changed ones removed just above).
	for app, ports := range desired {
		if _, ok := s.state[app]; ok {
			continue // already up to date
		}
		s.setupLocked(app, ports)
	}
}

func (s *InternalL4Set) setupLocked(app string, ports []L4Port) {
	vip, err := s.pool.Acquire()
	if err != nil {
		s.log.Error("internal-l4: VIP pool exhausted; app not exposed", "app", app, "err", err)
		return
	}
	if err := s.hooks.AddVIP(vip); err != nil {
		s.log.Error("internal-l4: add VIP failed", "app", app, "vip", vip, "err", err)
		s.pool.Release(vip)
		return
	}
	if err := s.hooks.OpenPorts(vip, tcpPortsOf(ports)); err != nil {
		s.log.Error("internal-l4: open nft ports failed", "app", app, "vip", vip, "err", err)
		_ = s.hooks.RemoveVIP(vip)
		s.pool.Release(vip)
		return
	}
	if err := s.hooks.Bind(app, vip, ports); err != nil {
		s.log.Error("internal-l4: bind proxy listeners failed", "app", app, "vip", vip, "err", err)
		_ = s.hooks.ClosePorts(vip, tcpPortsOf(ports))
		_ = s.hooks.RemoveVIP(vip)
		s.pool.Release(vip)
		return
	}
	s.state[app] = &binding{vip: vip, ports: ports}
	s.log.Info("internal-l4: app exposed", "app", app, "vip", vip, "ports", len(ports))
}

func (s *InternalL4Set) teardownLocked(app string, b *binding) {
	s.hooks.Unbind(app)
	if err := s.hooks.ClosePorts(b.vip, tcpPortsOf(b.ports)); err != nil {
		s.log.Warn("internal-l4: close nft ports failed", "app", app, "vip", b.vip, "err", err)
	}
	if err := s.hooks.RemoveVIP(b.vip); err != nil {
		s.log.Warn("internal-l4: remove VIP failed", "app", app, "vip", b.vip, "err", err)
	}
	s.pool.Release(b.vip)
	delete(s.state, app)
	s.log.Info("internal-l4: app un-exposed", "app", app, "vip", b.vip)
}

// Close tears every exposed app down (daemon shutdown).
func (s *InternalL4Set) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for app, b := range s.state {
		s.teardownLocked(app, b)
	}
}

/* ---- helpers ---- */

func sortedL4Ports(in []api.InternalPort) []L4Port {
	out := make([]L4Port, 0, len(in))
	for _, p := range in {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		out = append(out, L4Port{Port: p.Port, Proto: proto})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func tcpPortsOf(ports []L4Port) []int {
	out := make([]int, len(ports))
	for i, p := range ports {
		out[i] = p.Port // every L4 port is TCP at the network layer (http = L7 on top)
	}
	return out
}

func samePorts(a, b []L4Port) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
