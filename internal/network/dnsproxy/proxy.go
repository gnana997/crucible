package dnsproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/gnana997/crucible/internal/network"
)

// AllowIPFunc is the hook the proxy calls after a successful
// upstream resolution, once per A record in the answer section.
// The daemon wires it to network.AllowIP, which in turn pokes
// the per-sandbox nftables allowed-IPs set.
//
// Exposed as a function value (not an interface) so test code
// can pass a closure directly and assert call order without
// stubbing a type.
type AllowIPFunc func(ctx context.Context, sandboxID string, ip netip.Addr, ttl time.Duration) error

// Config bundles everything needed to construct a Proxy. Logger
// and Timeout have sensible zero values; ListenAddr, Upstream,
// and AllowIP are required.
type Config struct {
	// ListenAddr is the host-side address + port the proxy binds
	// UDP to. In production this is the reserved DNS anycast
	// (e.g. "10.20.255.254:53"); in tests, a 127.x.x.x loopback
	// address.
	ListenAddr string

	// Upstream is a dialable "ip:port" produced by ResolveUpstream.
	// The proxy treats it opaquely — if it's unreachable, queries
	// time out and the guest sees SERVFAIL.
	Upstream string

	// AllowIP is called once per answer-section A record. See
	// AllowIPFunc's doc. Required.
	AllowIP AllowIPFunc

	// UpstreamTimeout bounds one upstream query round-trip. Zero
	// means DefaultUpstreamTimeout.
	UpstreamTimeout time.Duration

	// AllowIPTimeout bounds the time spent updating the nft set
	// per A record. Zero means DefaultAllowIPTimeout.
	AllowIPTimeout time.Duration

	// Logger receives lifecycle events and per-query summaries.
	// Nil means slog.Default().
	Logger *slog.Logger
}

// Defaults picked to be tight enough that a wedged upstream
// doesn't hold a guest's DNS hostage, but loose enough that a
// slow but responsive resolver still works.
const (
	DefaultUpstreamTimeout = 5 * time.Second
	DefaultAllowIPTimeout  = 2 * time.Second
)

// Policy is the per-sandbox state the proxy consults on every
// query from that sandbox's source IP.
type Policy struct {
	// SandboxID is the identifier the proxy passes back to the
	// AllowIP hook so nft knows which set to update.
	SandboxID string

	// Allowlist answers "is this name allowed?" in O(labels).
	// The proxy does not mutate it.
	Allowlist *network.Allowlist
}

// Proxy is a running DNS enforcement server. Construct with
// Start, stop with Stop. Safe for concurrent Register /
// Deregister calls while ServeDNS is in flight.
type Proxy struct {
	cfg      Config
	srv      *dns.Server
	client   *dns.Client
	tcp      *dns.Client // fallback on truncation
	policies sync.Map    // key: netip.Addr (guest source IP), value: *Policy
	log      *slog.Logger

	started chan struct{}
	serveErr chan error
}

// Start binds a UDP listener at cfg.ListenAddr and spawns the
// serving goroutine. Returns once the listener is live (so the
// caller can safely Register policies and have them honored on
// the very next packet). Pre-bind failures return synchronously;
// post-bind failures surface via the error returned from Stop.
func Start(cfg Config) (*Proxy, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("dnsproxy: ListenAddr required")
	}
	if cfg.Upstream == "" {
		return nil, errors.New("dnsproxy: Upstream required")
	}
	if cfg.AllowIP == nil {
		return nil, errors.New("dnsproxy: AllowIP required")
	}
	if cfg.UpstreamTimeout == 0 {
		cfg.UpstreamTimeout = DefaultUpstreamTimeout
	}
	if cfg.AllowIPTimeout == 0 {
		cfg.AllowIPTimeout = DefaultAllowIPTimeout
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "dnsproxy", "listen", cfg.ListenAddr)

	p := &Proxy{
		cfg:      cfg,
		client:   &dns.Client{Net: "udp", Timeout: cfg.UpstreamTimeout},
		tcp:      &dns.Client{Net: "tcp", Timeout: cfg.UpstreamTimeout},
		log:      log,
		started:  make(chan struct{}),
		serveErr: make(chan error, 1),
	}

	// Pre-bind the PacketConn ourselves so we own the bind-failure
	// path (before spawning the goroutine). miekg/dns also supports
	// constructing the server with a pre-bound PacketConn directly,
	// which is what we do here.
	conn, err := net.ListenPacket("udp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("dnsproxy: bind %s: %w", cfg.ListenAddr, err)
	}

	p.srv = &dns.Server{
		PacketConn: conn,
		Handler:    p,
		NotifyStartedFunc: func() {
			close(p.started)
		},
	}

	go func() {
		if err := p.srv.ActivateAndServe(); err != nil {
			p.serveErr <- err
		}
		close(p.serveErr)
	}()

	// Wait for the server to announce readiness so Register calls
	// made immediately after Start take effect for the next query.
	select {
	case <-p.started:
	case err := <-p.serveErr:
		return nil, fmt.Errorf("dnsproxy: serve failed before start: %w", err)
	case <-time.After(5 * time.Second):
		_ = p.srv.Shutdown()
		return nil, errors.New("dnsproxy: server did not become ready within 5s")
	}
	log.Info("dns proxy started", "upstream", cfg.Upstream)
	return p, nil
}

// Stop gracefully shuts the listener down and waits for the serve
// goroutine to exit. Returns the serve loop's exit error, if any.
// Idempotent.
func (p *Proxy) Stop() error {
	err := p.srv.Shutdown()
	// Drain the serveErr channel (may have closed clean, or
	// carried a post-bind error).
	if serveErr, ok := <-p.serveErr; ok && err == nil {
		err = serveErr
	}
	return err
}

// Register associates a sandbox policy with a guest source IP.
// Calling Register twice with the same IP replaces the previous
// policy — useful when the guest's allowlist is updated without
// tearing down the proxy entry.
func (p *Proxy) Register(guestIP netip.Addr, pol *Policy) {
	if pol == nil || pol.Allowlist == nil {
		return
	}
	p.policies.Store(guestIP, pol)
}

// Deregister removes the policy for guestIP. Queries arriving
// from this IP afterward are dropped. Idempotent.
func (p *Proxy) Deregister(guestIP netip.Addr) {
	p.policies.Delete(guestIP)
}

// ServeDNS is the miekg/dns Handler entry. Implements the flow
// described in the package doc.
func (p *Proxy) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	srcIP, ok := sourceIP(w.RemoteAddr())
	if !ok {
		// Non-UDP or parse failure — drop silently. The proxy
		// only speaks UDP today; TCP fallback from clients would
		// arrive via a different Server instance if we ever wire
		// one.
		return
	}
	pol, ok := p.policyFor(srcIP)
	if !ok {
		// Unknown source. Could be a stray packet or a sandbox
		// whose policy was deregistered mid-query. Either way,
		// silent drop is the right answer — no NXDOMAIN, no log
		// (would be noisy under misconfiguration).
		return
	}

	// Evaluate questions. The DNS protocol allows multi-question
	// queries in theory but in practice every resolver sends
	// exactly one; we enforce all-or-nothing match so a crafted
	// multi-question message can't slip a denied name past us.
	for _, q := range req.Question {
		if !pol.Allowlist.Matches(q.Name) {
			p.log.Debug("denied", "sandbox", pol.SandboxID, "name", q.Name)
			p.writeRcode(w, req, dns.RcodeNameError)
			return
		}
	}

	// Forward to upstream. miekg/dns.Client handles EDNS0 for us
	// (preserves OPT records in the request/response) and honors
	// our configured timeout.
	reply, _, err := p.client.Exchange(req, p.cfg.Upstream)
	if err != nil {
		p.log.Warn("upstream error", "err", err, "upstream", p.cfg.Upstream)
		p.writeRcode(w, req, dns.RcodeServerFailure)
		return
	}
	// TC bit → retry over TCP so large responses (many A records,
	// DNSSEC chains) aren't truncated. This is the canonical
	// DNS-over-UDP fallback.
	if reply.Truncated {
		tcpReply, _, err := p.tcp.Exchange(req, p.cfg.Upstream)
		if err == nil {
			reply = tcpReply
		}
		// If TCP also fails, we serve the truncated UDP reply —
		// the client will see TC=1 and can retry itself.
	}

	// Walk the answer section for A records, feeding each to the
	// nft set with the record's TTL. Errors are logged but do
	// not fail the DNS response; a missed nft update is better
	// than a confused guest.
	for _, rr := range reply.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(a.A.To4())
		if !ok {
			continue
		}
		ttl := time.Duration(a.Hdr.Ttl) * time.Second
		if ttl <= 0 {
			ttl = time.Second // nft requires a positive timeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.AllowIPTimeout)
		if err := p.cfg.AllowIP(ctx, pol.SandboxID, ip, ttl); err != nil {
			p.log.Warn("nft allow-ip failed",
				"sandbox", pol.SandboxID, "ip", ip, "err", err)
		}
		cancel()
	}

	if err := w.WriteMsg(reply); err != nil {
		p.log.Debug("write reply failed", "err", err)
	}
}

// --- helpers ----------------------------------------------------

func (p *Proxy) policyFor(src netip.Addr) (*Policy, bool) {
	v, ok := p.policies.Load(src)
	if !ok {
		return nil, false
	}
	return v.(*Policy), true
}

// writeRcode builds a minimal response carrying only an RCODE
// and the original Question section, and writes it to w.
func (p *Proxy) writeRcode(w dns.ResponseWriter, req *dns.Msg, rcode int) {
	reply := new(dns.Msg)
	reply.SetRcode(req, rcode)
	if err := w.WriteMsg(reply); err != nil {
		p.log.Debug("write rcode failed", "rcode", rcode, "err", err)
	}
}

// sourceIP extracts the 4-octet IPv4 source from a
// net.Addr returned by a miekg/dns ResponseWriter. Returns
// ok=false for non-UDP, non-IPv4, or malformed addresses —
// caller drops in all those cases.
func sourceIP(a net.Addr) (netip.Addr, bool) {
	ua, ok := a.(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	v4 := ua.IP.To4()
	if v4 == nil {
		return netip.Addr{}, false
	}
	return netip.AddrFromSlice(v4)
}
