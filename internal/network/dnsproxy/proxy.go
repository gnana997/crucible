package dnsproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// AllowedIP is one range-vetted A record from an upstream reply:
// the resolved address plus the record's TTL (floored to 1s here;
// the nft layer clamps the upper bound).
type AllowedIP struct {
	Addr netip.Addr
	TTL  time.Duration
}

// AllowIPFunc is the hook the proxy calls after a successful
// upstream resolution, once per reply with every vetted A record
// from the answer section. The daemon wires it to
// network.AllowIPs, which pokes the per-sandbox nftables
// allowed-IPs set in a single nft invocation — batched per reply
// (not per record) so a fat DNS response can't force one process
// fork per A record.
//
// Exposed as a function value (not an interface) so test code
// can pass a closure directly and assert call order without
// stubbing a type.
type AllowIPFunc func(ctx context.Context, sandboxID string, ips []AllowedIP) error

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

	// AllowIP is called once per reply with the vetted A records.
	// See AllowIPFunc's doc. Required.
	AllowIP AllowIPFunc

	// BlockedPrefixes lists CIDR ranges whose A records are
	// stripped from replies and never passed to AllowIP, on top
	// of the built-in rejection of everything that isn't public
	// global-unicast. The manager passes the sandbox subnet pool
	// here so a pool configured outside RFC1918 space stays
	// unreachable too.
	BlockedPrefixes []netip.Prefix

	// UpstreamTimeout bounds one upstream query round-trip. Zero
	// means DefaultUpstreamTimeout.
	UpstreamTimeout time.Duration

	// AllowIPTimeout bounds the time spent updating the nft set
	// per reply. Zero means DefaultAllowIPTimeout.
	AllowIPTimeout time.Duration

	// MaxInflight caps concurrently served queries across all
	// sandboxes; packets beyond it are dropped before any work is
	// done. Zero means DefaultMaxInflight.
	MaxInflight int

	// SourceQPS is the sustained per-source query rate (burst is
	// twice this); queries beyond it are dropped. Zero means
	// DefaultSourceQPS.
	SourceQPS int

	// PerSourceInflight caps how many upstream round-trips a single
	// source (sandbox) may have in flight at once. It bounds each
	// guest's share of the global MaxInflight pool so one guest
	// resolving a slow — or deliberately stalling, attacker-run —
	// authoritative server can't hold most of the global slots for
	// UpstreamTimeout apiece and starve every other sandbox's DNS.
	// Zero means DefaultPerSourceInflight.
	PerSourceInflight int

	// Logger receives lifecycle events and per-query summaries.
	// Nil means slog.Default().
	Logger *slog.Logger

	// InternalZone, when non-empty (e.g. "internal"), makes the proxy answer
	// <app>.<InternalZone> queries authoritatively with InternalVIP (the ingress
	// anycast VIP) instead of forwarding upstream — app→app service discovery
	// (v0.5.1). The proxy does not verify the app exists; the ingress proxy routes
	// or 404s and (in a later slice) enforces per-app call authorization.
	InternalZone string

	// InternalVIP is the A address returned for InternalZone queries in LEGACY mode
	// — the shared DNS anycast, which the ingress L7 proxy also listens on. Used
	// only when InternalVIPForApp is nil. Ignored when InternalZone is empty or
	// InternalVIP is invalid.
	InternalVIP netip.Addr

	// InternalVIPForApp, when set, resolves an in-zone query to the target app's
	// OWN per-app VIP (v0.9.5 L4 mode) instead of the shared anycast — so
	// <app>.internal routes by destination IP and can carry raw TCP. It takes
	// precedence over InternalVIP. Returning ok=false (the app has no VIP: unknown,
	// or L4 not enabled for it) yields NXDOMAIN — same fail-closed, no-inventory-leak
	// behavior as an unauthorized lookup. Consulted only AFTER InternalAuthz passes.
	InternalVIPForApp func(app string) (netip.Addr, bool)

	// InternalAuthz authorizes an internal-zone lookup: given the querying guest's
	// source IP and the target app, may the caller reach it? When InternalZone is
	// set this MUST be set too — a nil authorizer denies every internal lookup
	// (NXDOMAIN), so a guest can't even discover an app it isn't granted (no
	// inventory leak). Keyed by source IP (not sandbox id) so it uses the same
	// caller→app mapping as the ingress proxy, which remains the enforcing
	// boundary; this mirrors its decision at the DNS layer.
	InternalAuthz func(callerIP, targetApp string) bool
}

// Defaults picked to be tight enough that a wedged upstream
// doesn't hold a guest's DNS hostage, but loose enough that a
// slow but responsive resolver still works.
const (
	DefaultUpstreamTimeout = 5 * time.Second
	DefaultAllowIPTimeout  = 2 * time.Second

	// DefaultMaxInflight bounds handler concurrency. miekg/dns
	// spawns one goroutine per packet with no cap of its own, so
	// this semaphore is what stands between a guest's UDP flood
	// and unbounded goroutines each holding an upstream socket.
	DefaultMaxInflight = 64

	// DefaultSourceQPS is the sustained per-sandbox query rate.
	// Generous for real workloads (package installs burst well
	// below this) while keeping one guest from monopolizing the
	// inflight slots.
	DefaultSourceQPS = 50

	// DefaultPerSourceInflight bounds a single source's concurrent
	// upstream round-trips. Small enough that no one guest can pin a
	// large share of DefaultMaxInflight, large enough that a normal
	// fan-out of parallel lookups (a build resolving several hosts at
	// once) isn't throttled — for well-behaved fast upstreams the
	// per-source QPS limit is the tighter bound anyway.
	DefaultPerSourceInflight = 8

	// maxAnswerIPs caps how many A records of a single reply are
	// returned to the guest and passed to AllowIP. Legitimate
	// answers rarely carry more than a handful; a 64 KB TCP-
	// fallback reply can carry thousands.
	maxAnswerIPs = 16
)

// Matcher is the minimal interface the DNS proxy needs to
// decide whether a query is allowed. Defined here (not imported
// from internal/network) to keep this package dependency-free
// on its parent — the parent is free to import back without
// creating a cycle.
//
// In production the implementation is *network.Allowlist; in
// tests it's commonly a custom stub.
type Matcher interface {
	Matches(name string) bool
}

// Policy is the per-sandbox state the proxy consults on every
// query from that sandbox's source IP.
type Policy struct {
	// SandboxID is the identifier the proxy passes back to the
	// AllowIP hook so nft knows which set to update.
	SandboxID string

	// Allowlist answers "is this name allowed?" in O(labels).
	// The proxy does not mutate it.
	Allowlist Matcher
}

// Proxy is a running DNS enforcement server. Construct with
// Start, stop with Stop. Safe for concurrent Register /
// Deregister calls while ServeDNS is in flight.
type Proxy struct {
	cfg      Config
	srv      *dns.Server
	client   *dns.Client
	tcp      *dns.Client   // fallback on truncation
	policies sync.Map      // key: netip.Addr (guest source IP), value: *policyEntry
	sem      chan struct{} // inflight-handler semaphore
	log      *slog.Logger

	started  chan struct{}
	serveErr chan error
}

// policyEntry pairs the caller's Policy with the proxy-internal
// per-source rate limiter and upstream in-flight cap.
type policyEntry struct {
	pol      *Policy
	lim      *rateLimiter
	inflight chan struct{} // bounds this source's concurrent upstream round-trips
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
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = DefaultMaxInflight
	}
	if cfg.SourceQPS <= 0 {
		cfg.SourceQPS = DefaultSourceQPS
	}
	if cfg.PerSourceInflight <= 0 {
		cfg.PerSourceInflight = DefaultPerSourceInflight
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
		sem:      make(chan struct{}, cfg.MaxInflight),
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
//
// pol.Allowlist must satisfy Matcher; a nil Allowlist is rejected
// (would silently deny everything, which callers can express more
// clearly by simply not Registering).
func (p *Proxy) Register(guestIP netip.Addr, pol *Policy) {
	if pol == nil || pol.Allowlist == nil {
		return
	}
	qps := p.cfg.SourceQPS
	if qps <= 0 {
		qps = DefaultSourceQPS
	}
	inflight := p.cfg.PerSourceInflight
	if inflight <= 0 {
		inflight = DefaultPerSourceInflight
	}
	p.policies.Store(guestIP, &policyEntry{
		pol:      pol,
		lim:      newRateLimiter(float64(qps), float64(2*qps)),
		inflight: make(chan struct{}, inflight),
	})
}

// Deregister removes the policy for guestIP. Queries arriving
// from this IP afterward are dropped. Idempotent.
func (p *Proxy) Deregister(guestIP netip.Addr) {
	p.policies.Delete(guestIP)
}

// ServeDNS is the miekg/dns Handler entry. Implements the flow
// described in the package doc.
func (p *Proxy) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	// Concurrency gate first. miekg/dns has already spawned this
	// goroutine (one per packet, uncapped); what the semaphore
	// bounds is the expensive part — upstream sockets and nft
	// invocations — so a UDP flood sheds load here instead of
	// exhausting the host.
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		return // saturated: drop, resolver retries
	}

	srcIP, ok := sourceIP(w.RemoteAddr())
	if !ok {
		// Non-UDP or parse failure — drop silently. The proxy
		// only speaks UDP today; TCP fallback from clients would
		// arrive via a different Server instance if we ever wire
		// one.
		return
	}
	ent, ok := p.policyFor(srcIP)
	if !ok {
		// Unknown source. Could be a stray packet or a sandbox
		// whose policy was deregistered mid-query. Either way,
		// silent drop is the right answer — no NXDOMAIN, no log
		// (would be noisy under misconfiguration).
		return
	}
	pol := ent.pol
	// Per-source rate limit, before any real work. Silent drop
	// like unknown sources — a reply would just be amplification
	// during a flood.
	if !ent.lim.allow(time.Now()) {
		return
	}

	// App→app (v0.5.1): answer <app>.<InternalZone> authoritatively with the
	// ingress VIP, bypassing the egress allowlist + upstream. The name is not in
	// any guest's egress allowlist by design, so this branch must run before the
	// allowlist check below.
	if reply := p.internalAnswer(req, srcIP.String()); reply != nil {
		if err := w.WriteMsg(reply); err != nil {
			p.log.Debug("write internal reply failed", "err", err)
		}
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

	// Per-source in-flight cap, acquired after the cheap
	// rate-limit/allowlist checks and immediately before the slow
	// upstream round-trip. The global p.sem bounds total concurrency;
	// this bounds any single source's share of it, so a guest
	// resolving an allowlisted-but-slow (or attacker-run,
	// deliberately-stalling) authoritative server can't pin most of
	// the global slots for UpstreamTimeout each and black out every
	// other sandbox's DNS. Released when the handler returns.
	select {
	case ent.inflight <- struct{}{}:
		defer func() { <-ent.inflight }()
	default:
		return // source already at PerSourceInflight; resolver retries
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

	// Strip AAAA records from the reply. The sandbox network is
	// IPv4-only (nft allowlist is ipv4_addr-only, veth + tap have
	// no IPv6), so returning AAAA would actively hurt: glibc's
	// gethostbyname2/getaddrinfo prefers IPv6 when both families
	// are available, so clients would attempt unreachable IPv6
	// and never fall back to the A records that actually work.
	// Stripping AAAA makes the guest believe the host has no IPv6
	// and use IPv4 unconditionally.
	reply.Answer = filterOutAAAA(reply.Answer)
	reply.Extra = filterOutAAAA(reply.Extra)

	// Walk the answer section: vet each A record's address range,
	// cap the count, and collect the survivors for one batched
	// AllowIP call. Records that fail vetting are stripped from
	// the reply as well — the guest must never see an address the
	// allow-set won't open, and the allow-set must never open
	// link-local (cloud metadata), private, or sandbox-pool space
	// no matter what an attacker-controlled upstream answers.
	var allowed []AllowedIP
	seen := make(map[netip.Addr]bool)
	kept := reply.Answer[:0]
	for _, rr := range reply.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			kept = append(kept, rr)
			continue
		}
		ip, ok := netip.AddrFromSlice(a.A.To4())
		if !ok || p.blockedIP(ip) {
			p.log.Debug("stripped unroutable A record",
				"sandbox", pol.SandboxID, "name", a.Hdr.Name, "ip", a.A)
			continue
		}
		if len(allowed) >= maxAnswerIPs {
			continue
		}
		kept = append(kept, rr)
		if seen[ip] {
			continue
		}
		seen[ip] = true
		ttl := time.Duration(a.Hdr.Ttl) * time.Second
		if ttl <= 0 {
			ttl = time.Second // nft requires a positive timeout
		}
		allowed = append(allowed, AllowedIP{Addr: ip, TTL: ttl})
	}
	reply.Answer = kept

	// One nft invocation per reply, with the record TTLs. Errors
	// are logged but do not fail the DNS response; a missed nft
	// update is better than a confused guest.
	if len(allowed) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.AllowIPTimeout)
		if err := p.cfg.AllowIP(ctx, pol.SandboxID, allowed); err != nil {
			p.log.Warn("nft allow-ip failed",
				"sandbox", pol.SandboxID, "ips", len(allowed), "err", err)
		}
		cancel()
	}

	if err := w.WriteMsg(reply); err != nil {
		p.log.Debug("write reply failed", "err", err)
	}
}

// ReservedV4Prefixes exposes reservedV4Prefixes so the nft layer
// (network.BlockedEgressPrefixes) can build its drop rules from the same
// source of truth the DNS answer filter uses — see IsPublicUnicast.
func ReservedV4Prefixes() []netip.Prefix {
	return append([]netip.Prefix(nil), reservedV4Prefixes...)
}

// reservedV4Prefixes are IANA special-purpose IPv4 blocks that
// netip's IsGlobalUnicast/IsPrivate do NOT catch: they look like
// ordinary global unicast (and aren't RFC1918) yet route to
// carrier-grade NAT — where several clouds put their instance
// metadata service, e.g. Alibaba's 100.100.100.200 — or to
// documentation, benchmarking, and reserved future-use space. A
// DNS-resolved egress target inside any of these must be refused the
// same as link-local (169.254.169.254) or RFC1918. Loopback,
// link-local, multicast, and the unspecified/broadcast addresses are
// already rejected by IsGlobalUnicast and are not repeated here.
var reservedV4Prefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (RFC 1122 §3.2.1.3)
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space / CGNAT (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments (incl. NAT64/DS-Lite)
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 (RFC 5737)
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast (RFC 7526, deprecated)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 (RFC 5737)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved / future use (RFC 1112 §4)
}

// IsPublicUnicast reports whether ip is safe to expose as a DNS
// egress target: a public global-unicast address that is not RFC1918
// private and not inside any reservedV4Prefixes block. It is the
// single source of truth for "public" — the proxy's answer filter
// (blockedIP) and network.AllowIPs both call it, so the layer that
// strips records from the guest's reply and the layer that pokes the
// nftables allow-set can never disagree about which addresses are
// reachable. Anything it rejects reaches neither the guest nor the
// allow-set.
func IsPublicUnicast(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, pfx := range reservedV4Prefixes {
		if pfx.Contains(ip) {
			return false
		}
	}
	return true
}

// blockedIP reports whether a resolved address must never reach a
// guest or its egress allow-set: everything IsPublicUnicast rejects
// (non-global-unicast, RFC1918, and the IANA special-purpose ranges
// including link-local cloud metadata and CGNAT) plus any
// operator-configured Config.BlockedPrefixes (e.g. a sandbox pool
// placed outside RFC1918).
func (p *Proxy) blockedIP(ip netip.Addr) bool {
	if !IsPublicUnicast(ip) {
		return true
	}
	for _, pfx := range p.cfg.BlockedPrefixes {
		if pfx.Contains(ip) {
			return true
		}
	}
	return false
}

// filterOutAAAA returns rrs with every *dns.AAAA removed. Other
// RR types pass through untouched.
func filterOutAAAA(rrs []dns.RR) []dns.RR {
	out := rrs[:0]
	for _, rr := range rrs {
		if _, isAAAA := rr.(*dns.AAAA); isAAAA {
			continue
		}
		out = append(out, rr)
	}
	return out
}

// --- helpers ----------------------------------------------------

// internalRecordTTL is the TTL (seconds) on synthesized app→app A records. The
// VIP is a fixed anycast, so this only bounds how fast disabling the zone
// propagates to guests; keep it modest.
const internalRecordTTL = 30

// internalAnswer returns an authoritative reply for an app→app query in the
// configured internal zone, or nil when the zone is disabled or the query is not
// under it. A queries return the ingress VIP; AAAA (and other qtypes) get an
// empty NOERROR (NODATA) so a dual-stack resolver falls back to the A record on
// the IPv4-only guest fabric.
func (p *Proxy) internalAnswer(req *dns.Msg, callerIP string) *dns.Msg {
	// Need a way to resolve an in-zone name: a per-app VIP lookup (v0.9.5 L4 mode)
	// or the shared anycast VIP (legacy). Neither wired → not ours.
	if p.cfg.InternalZone == "" || len(req.Question) != 1 {
		return nil
	}
	if p.cfg.InternalVIPForApp == nil && !p.cfg.InternalVIP.IsValid() {
		return nil
	}
	q := req.Question[0]
	if q.Qclass != dns.ClassINET {
		return nil
	}
	zone := dns.CanonicalName(dns.Fqdn(p.cfg.InternalZone))
	qn := dns.CanonicalName(q.Name)
	if qn == zone || !dns.IsSubDomain(zone, qn) {
		return nil // bare zone or out-of-zone → not ours
	}
	// Default-deny: the target app is the label(s) left of the zone. If the caller
	// isn't authorized (or no authorizer is wired), return NXDOMAIN so a guest
	// can't even discover an app it may not call.
	target := strings.TrimSuffix(strings.TrimSuffix(qn, zone), ".")
	if p.cfg.InternalAuthz == nil || !p.cfg.InternalAuthz(callerIP, target) {
		return internalNameError(req)
	}
	// Resolve the address: per-app VIP (L4) takes precedence over the shared anycast.
	// An authorized app with no VIP is NXDOMAIN (unknown or L4-not-enabled) — same
	// fail-closed, no-leak behavior as an unauthorized lookup.
	vip := p.cfg.InternalVIP
	if p.cfg.InternalVIPForApp != nil {
		v, ok := p.cfg.InternalVIPForApp(target)
		if !ok {
			return internalNameError(req)
		}
		vip = v
	}
	if !vip.IsValid() {
		return nil
	}
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	if q.Qtype == dns.TypeA {
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: internalRecordTTL},
			A:   net.IP(vip.AsSlice()),
		})
	}
	return m
}

// internalNameError builds an authoritative NXDOMAIN reply for an in-zone query the
// caller may not resolve (unauthorized, or an app with no VIP) — fail-closed with no
// inventory leak.
func internalNameError(req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(req, dns.RcodeNameError)
	m.Authoritative = true
	return m
}

func (p *Proxy) policyFor(src netip.Addr) (*policyEntry, bool) {
	v, ok := p.policies.Load(src)
	if !ok {
		return nil, false
	}
	return v.(*policyEntry), true
}

// rateLimiter is a minimal token bucket, one per registered
// source. (golang.org/x/time is not a dependency; this is the
// small subset we need.)
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // tokens replenished per second
	burst  float64 // bucket capacity
	tokens float64
	last   time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{rate: rate, burst: burst, tokens: burst}
}

func (l *rateLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
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
