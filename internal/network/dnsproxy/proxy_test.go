package dnsproxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// stubMatcher satisfies the Matcher interface for tests, matching
// if the query name is in a pre-configured set (case-insensitive,
// trailing-dot-tolerant). Keeps this package test-only so we
// don't need to import the parent network package (which would
// create a cycle).
type stubMatcher struct {
	allowed map[string]bool
}

func newStubMatcher(patterns ...string) *stubMatcher {
	m := &stubMatcher{allowed: make(map[string]bool, len(patterns))}
	for _, p := range patterns {
		m.allowed[strings.TrimSuffix(strings.ToLower(p), ".")] = true
	}
	return m
}

func (m *stubMatcher) Matches(name string) bool {
	key := strings.TrimSuffix(strings.ToLower(name), ".")
	if m.allowed[key] {
		return true
	}
	// Cheap single-label wildcard: for each pattern like
	// "*.suffix.com", accept anything ending ".suffix.com"
	// (exactly one extra label).
	for p := range m.allowed {
		if !strings.HasPrefix(p, "*.") {
			continue
		}
		suffix := p[1:] // ".suffix.com"
		if !strings.HasSuffix(key, suffix) {
			continue
		}
		head := strings.TrimSuffix(key, suffix)
		if head != "" && !strings.Contains(head, ".") {
			return true
		}
	}
	return false
}

// Tests use three different 127.x.x.x loopback addresses:
//
//   - proxy bind     127.0.0.2
//   - upstream stub  127.0.0.3
//   - guest source   127.0.0.5 (forged by binding the client
//     socket's LocalAddr to it)
//
// This lets us exercise the source-IP-based policy lookup
// without root or netns tricks — all we need is that all three
// addresses are in the loopback range (which 127/8 guarantees
// on Linux).

const (
	proxyIP    = "127.0.0.2"
	upstreamIP = "127.0.0.3"
	guestIP    = "127.0.0.5" // registered
	strayIP    = "127.0.0.9" // not registered
)

// startStubUpstream spins up a miekg/dns server on 127.0.0.3 that
// answers any A query with the canned records. Returns its address
// and a counter that tests can assert on to verify the proxy
// actually forwarded (vs. short-circuiting).
func startStubUpstream(t *testing.T, ttl uint32, answerIPs ...string) (string, *atomic.Int64) {
	t.Helper()
	return startStubUpstreamDelay(t, 0, ttl, answerIPs...)
}

// startStubUpstreamDelay is startStubUpstream with an artificial
// per-query delay, for tests that need handlers to pile up.
func startStubUpstreamDelay(t *testing.T, delay time.Duration, ttl uint32, answerIPs ...string) (string, *atomic.Int64) {
	t.Helper()
	var count atomic.Int64

	addr := upstreamIP + ":0"
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("upstream bind: %v", err)
	}
	bound := conn.LocalAddr().String()

	srv := &dns.Server{
		PacketConn: conn,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			count.Add(1)
			if delay > 0 {
				time.Sleep(delay)
			}
			reply := new(dns.Msg)
			reply.SetReply(r)
			// Compress so many-record replies stay under the plain-
			// UDP 512-byte limit (test clients don't send EDNS0).
			reply.Compress = true
			for _, q := range r.Question {
				if q.Qtype != dns.TypeA {
					continue
				}
				for _, ip := range answerIPs {
					reply.Answer = append(reply.Answer, &dns.A{
						Hdr: dns.RR_Header{
							Name:   q.Name,
							Rrtype: dns.TypeA,
							Class:  dns.ClassINET,
							Ttl:    ttl,
						},
						A: net.ParseIP(ip).To4(),
					})
				}
			}
			_ = w.WriteMsg(reply)
		}),
	}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started

	t.Cleanup(func() { _ = srv.Shutdown() })
	return bound, &count
}

// startTestProxy brings up a Proxy on 127.0.0.2 talking to the
// given upstream. Registers a policy for the standard guestIP
// with the supplied allowlist patterns. Returns the proxy and
// an allowIP call log so tests can assert nft updates.
func startTestProxy(t *testing.T, upstream string, patterns []string) (*Proxy, string, *allowIPRecorder) {
	t.Helper()
	return startTestProxyCfg(t, upstream, patterns, nil)
}

// startTestProxyCfg is startTestProxy with a Config-mutating hook
// for tests exercising non-default limits/prefixes.
func startTestProxyCfg(t *testing.T, upstream string, patterns []string, mutate func(*Config)) (*Proxy, string, *allowIPRecorder) {
	t.Helper()
	recorder := newAllowIPRecorder()

	// bind :0 to get a free port
	listener, err := net.ListenPacket("udp", proxyIP+":0")
	if err != nil {
		t.Fatalf("proxy pre-bind: %v", err)
	}
	proxyAddr := listener.LocalAddr().String()
	_ = listener.Close() // release so Start can bind

	cfg := Config{
		ListenAddr: proxyAddr,
		Upstream:   upstream,
		AllowIP:    recorder.record,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	p.Register(mustAddr(t, guestIP), &Policy{
		SandboxID: "sbx-test",
		Allowlist: newStubMatcher(patterns...),
	})

	return p, proxyAddr, recorder
}

// sendFromGuest sends a DNS query to the proxy with the source
// address set to `src` (a loopback 127.x.x.x). The DNS client
// uses a Dialer with LocalAddr set so the kernel picks the given
// source IP — this is the cheap trick that replaces per-sandbox
// source-IP synthesis.
func sendFromGuest(t *testing.T, proxyAddr, src, qname string, qtype uint16) (*dns.Msg, error) {
	t.Helper()
	c := &dns.Client{
		Net:     "udp",
		Timeout: 3 * time.Second,
		Dialer: &net.Dialer{
			LocalAddr: &net.UDPAddr{IP: net.ParseIP(src), Port: 0},
			Timeout:   3 * time.Second,
		},
	}
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(qname), qtype)
	resp, _, err := c.Exchange(req, proxyAddr)
	return resp, err
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// --- AllowIP call log -------------------------------------------

type allowIPCall struct {
	SandboxID string
	IP        netip.Addr
	TTL       time.Duration
}

type allowIPRecorder struct {
	mu      sync.Mutex
	batches int // one per hook invocation (i.e. per reply)
	calls   []allowIPCall
}

func newAllowIPRecorder() *allowIPRecorder { return &allowIPRecorder{} }

func (r *allowIPRecorder) record(_ context.Context, sandboxID string, ips []AllowedIP) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches++
	for _, e := range ips {
		r.calls = append(r.calls, allowIPCall{SandboxID: sandboxID, IP: e.Addr, TTL: e.TTL})
	}
	return nil
}

func (r *allowIPRecorder) batchCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.batches
}

func (r *allowIPRecorder) snapshot() []allowIPCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]allowIPCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// --- tests ------------------------------------------------------

// TestStartReturnsReadyProxyImmediately pins the readiness guarantee that made
// the former Manager.WaitForDNSProxyReady redundant: Start returns only
// after the UDP listener is bound and the server's NotifyStartedFunc has fired,
// so a query sent the instant Start returns — no wait, no retry — is answered.
func TestStartReturnsReadyProxyImmediately(t *testing.T) {
	upstream, _ := startStubUpstream(t, 60, "93.184.216.34")

	listener, err := net.ListenPacket("udp", proxyIP+":0")
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	proxyAddr := listener.LocalAddr().String()
	_ = listener.Close()

	p, err := Start(Config{ListenAddr: proxyAddr, Upstream: upstream, AllowIP: func(context.Context, string, []AllowedIP) error { return nil }})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	// No sleep/poll between Start and Register+query: if Start returned before
	// the listener was live, this Exchange would time out.
	p.Register(mustAddr(t, guestIP), &Policy{SandboxID: "sbx-test", Allowlist: newStubMatcher("example.com")})

	resp, err := sendFromGuest(t, proxyAddr, guestIP, "example.com", dns.TypeA)
	if err != nil {
		t.Fatalf("query immediately after Start: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("Rcode=%d answers=%d, want NOERROR with 1 answer", resp.Rcode, len(resp.Answer))
	}
}

func TestProxyAllowedQueryForwardsAndPokesNFT(t *testing.T) {
	upstream, upstreamHits := startStubUpstream(t, 60, "93.184.216.34")
	_, proxyAddr, recorder := startTestProxy(t, upstream, []string{"example.com"})

	resp, err := sendFromGuest(t, proxyAddr, guestIP, "example.com", dns.TypeA)
	if err != nil {
		t.Fatalf("client Exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("Rcode = %d, want NOERROR", resp.Rcode)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(resp.Answer))
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("upstream hit count = %d, want 1", upstreamHits.Load())
	}

	// Give the proxy a moment to finish the async-ish AllowIP
	// call before we snapshot.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(recorder.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := recorder.snapshot()
	if len(calls) != 1 {
		t.Fatalf("AllowIP calls = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].SandboxID != "sbx-test" {
		t.Errorf("SandboxID = %q", calls[0].SandboxID)
	}
	if calls[0].IP.String() != "93.184.216.34" {
		t.Errorf("IP = %s, want 93.184.216.34", calls[0].IP)
	}
	if calls[0].TTL != 60*time.Second {
		t.Errorf("TTL = %v, want 60s", calls[0].TTL)
	}
}

func TestProxyDeniedQueryReturnsNXDOMAIN(t *testing.T) {
	// Upstream shouldn't be consulted — assert its counter stays 0.
	upstream, upstreamHits := startStubUpstream(t, 60, "93.184.216.34")
	_, proxyAddr, recorder := startTestProxy(t, upstream, []string{"pypi.org"})

	resp, err := sendFromGuest(t, proxyAddr, guestIP, "evil.example.com", dns.TypeA)
	if err != nil {
		t.Fatalf("client Exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeNameError {
		t.Errorf("Rcode = %d, want NXDOMAIN", resp.Rcode)
	}
	if len(resp.Answer) != 0 {
		t.Errorf("answer section should be empty, got %d records", len(resp.Answer))
	}
	if upstreamHits.Load() != 0 {
		t.Errorf("upstream hit on denied query (%d) — denial must short-circuit", upstreamHits.Load())
	}
	if len(recorder.snapshot()) != 0 {
		t.Errorf("nft AllowIP called on denied query: %+v", recorder.snapshot())
	}
}

func TestProxyUnknownSourceIsDropped(t *testing.T) {
	upstream, _ := startStubUpstream(t, 60, "93.184.216.34")
	_, proxyAddr, _ := startTestProxy(t, upstream, []string{"example.com"})

	// Use a source IP we did NOT register. Proxy should drop
	// silently; client sees a timeout, not a reply.
	c := &dns.Client{
		Net:     "udp",
		Timeout: 500 * time.Millisecond,
		Dialer: &net.Dialer{
			LocalAddr: &net.UDPAddr{IP: net.ParseIP(strayIP), Port: 0},
			Timeout:   500 * time.Millisecond,
		},
	}
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn("example.com"), dns.TypeA)
	_, _, err := c.Exchange(req, proxyAddr)
	if err == nil {
		t.Fatal("expected timeout for unknown source; got a response")
	}
}

func TestProxyWildcardAllowlist(t *testing.T) {
	upstream, _ := startStubUpstream(t, 30, "104.16.1.1")
	_, proxyAddr, recorder := startTestProxy(t, upstream, []string{"*.npmjs.org"})

	resp, err := sendFromGuest(t, proxyAddr, guestIP, "registry.npmjs.org", dns.TypeA)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("wildcard should have allowed the query, got Rcode=%d", resp.Rcode)
	}

	// Apex doesn't match *.npmjs.org per single-label wildcard rules.
	resp2, err := sendFromGuest(t, proxyAddr, guestIP, "npmjs.org", dns.TypeA)
	if err != nil {
		t.Fatalf("Exchange apex: %v", err)
	}
	if resp2.Rcode != dns.RcodeNameError {
		t.Errorf("apex should be denied under single-label wildcard, got Rcode=%d", resp2.Rcode)
	}

	// Sanity: recorder shows exactly one call (for the wildcard match).
	if got := len(recorder.snapshot()); got != 1 {
		t.Errorf("recorder calls = %d, want 1", got)
	}
}

func TestProxyDeregisterStopsAcceptingQueries(t *testing.T) {
	upstream, _ := startStubUpstream(t, 30, "1.2.3.4")
	p, proxyAddr, _ := startTestProxy(t, upstream, []string{"example.com"})

	// Warmup: allowed query works.
	resp, err := sendFromGuest(t, proxyAddr, guestIP, "example.com", dns.TypeA)
	if err != nil || resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("warmup: err=%v rcode=%d", err, resp.Rcode)
	}

	// Deregister.
	p.Deregister(mustAddr(t, guestIP))

	// Now same source+name should time out (drop).
	c := &dns.Client{
		Net:     "udp",
		Timeout: 500 * time.Millisecond,
		Dialer: &net.Dialer{
			LocalAddr: &net.UDPAddr{IP: net.ParseIP(guestIP), Port: 0},
			Timeout:   500 * time.Millisecond,
		},
	}
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn("example.com"), dns.TypeA)
	_, _, err = c.Exchange(req, proxyAddr)
	if err == nil {
		t.Fatal("expected timeout after Deregister")
	}
}

func TestProxyUpstreamFailureReturnsServfail(t *testing.T) {
	// Point upstream at a port where nothing's listening. The
	// Client should fail fast and we should return SERVFAIL.
	dead := "127.0.0.3:1"
	_, proxyAddr, _ := startTestProxy(t, dead, []string{"example.com"})

	resp, err := sendFromGuest(t, proxyAddr, guestIP, "example.com", dns.TypeA)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeServerFailure {
		t.Errorf("Rcode = %d, want SERVFAIL", resp.Rcode)
	}
}

func TestStartRejectsEmptyConfig(t *testing.T) {
	cases := []Config{
		{Upstream: "1.1.1.1:53", AllowIP: func(context.Context, string, []AllowedIP) error { return nil }},
		{ListenAddr: "127.0.0.2:0", AllowIP: func(context.Context, string, []AllowedIP) error { return nil }},
		{ListenAddr: "127.0.0.2:0", Upstream: "1.1.1.1:53"},
	}
	for i, cfg := range cases {
		if _, err := Start(cfg); err == nil {
			t.Errorf("case %d: expected error for incomplete config", i)
		}
	}
}

func TestStartFailsOnUnbindableAddress(t *testing.T) {
	// 240.0.0.1 is in the Class E reserved range — binding to it
	// fails on Linux with EADDRNOTAVAIL, which exercises our
	// pre-bind error path.
	_, err := Start(Config{
		ListenAddr: "240.0.0.1:53",
		Upstream:   "1.1.1.1:53",
		AllowIP:    func(context.Context, string, []AllowedIP) error { return nil },
	})
	if err == nil {
		t.Fatal("expected bind error for unbindable address")
	}
}

func TestPolicyForSourceIPLookup(t *testing.T) {
	// Direct micro-test of the internal sync.Map path, for when
	// integration tests get flaky under load.
	_, _, _ = startTestProxy(t, "127.0.0.3:1", []string{"x.com"})

	// We re-use startTestProxy's side effect (registers guestIP)
	// but reach into a fresh proxy for direct introspection.
	upstream, _ := startStubUpstream(t, 30, "1.2.3.4")
	p, _, _ := startTestProxy(t, upstream, []string{"x.com"})

	if _, ok := p.policyFor(mustAddr(t, guestIP)); !ok {
		t.Error("registered IP not found")
	}
	if _, ok := p.policyFor(mustAddr(t, "127.99.99.99")); ok {
		t.Error("unregistered IP returned a policy")
	}
}

func TestRegisterIgnoresNilPolicy(t *testing.T) {
	p := &Proxy{}
	p.Register(mustAddr(t, "127.0.0.1"), nil)
	if _, ok := p.policies.Load(mustAddr(t, "127.0.0.1")); ok {
		t.Error("nil policy should not be stored")
	}

	// Also: policy with nil Allowlist.
	p.Register(mustAddr(t, "127.0.0.1"), &Policy{SandboxID: "x"})
	if _, ok := p.policies.Load(mustAddr(t, "127.0.0.1")); ok {
		t.Error("policy with nil Allowlist should not be stored")
	}
}

func TestSourceIPHelper(t *testing.T) {
	ok := []struct {
		addr net.Addr
		want string
	}{
		{&net.UDPAddr{IP: net.IPv4(10, 20, 0, 3), Port: 53}, "10.20.0.3"},
		{&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 999}, "127.0.0.1"},
	}
	for _, c := range ok {
		got, ok := sourceIP(c.addr)
		if !ok {
			t.Errorf("%v: sourceIP returned !ok", c.addr)
			continue
		}
		if got.String() != c.want {
			t.Errorf("%v: got %q, want %q", c.addr, got, c.want)
		}
	}

	bad := []net.Addr{
		&net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1},
		&net.UDPAddr{IP: net.ParseIP("::1"), Port: 1}, // IPv6 — v0.1 is IPv4-only
	}
	for _, b := range bad {
		if _, ok := sourceIP(b); ok {
			t.Errorf("%v: expected !ok", b)
		}
	}

	_ = fmt.Sprintf // silence unused import in case we trim stubs later
}

// TestProxyStripsUnroutableAnswers is the regression test: an
// attacker-controlled upstream answering with metadata, sandbox-
// pool, LAN, or other non-public addresses must get NOTHING — no
// allow-set update, no record returned to the guest.
func TestProxyStripsUnroutableAnswers(t *testing.T) {
	cases := []string{
		"169.254.169.254", // cloud metadata (link-local)
		"10.20.3.2",       // sandbox pool (also RFC1918)
		"192.168.1.10",    // host LAN
		"127.0.0.1",       // loopback
		"224.0.0.1",       // multicast
		"255.255.255.255", // broadcast
		"0.0.0.0",         // unspecified
		// IANA special-purpose ranges that look like global unicast
		// but must never be an egress target (R1).
		"100.100.100.200", // Alibaba cloud metadata (CGNAT / RFC 6598)
		"100.64.0.1",      // CGNAT low edge
		"100.127.255.255", // CGNAT high edge
		"0.1.2.3",         // 0.0.0.0/8 "this network" (non-zero host)
		"192.0.0.171",     // NAT64/DS-Lite (IETF protocol assignments)
		"192.0.2.7",       // TEST-NET-1 documentation
		"192.88.99.1",     // 6to4 relay anycast (deprecated)
		"198.18.0.5",      // benchmarking (RFC 2544)
		"198.51.100.9",    // TEST-NET-2 documentation
		"203.0.113.9",     // TEST-NET-3 documentation
		"240.0.0.1",       // reserved / future use (class E)
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			upstream, _ := startStubUpstream(t, 60, bad)
			_, proxyAddr, recorder := startTestProxy(t, upstream, []string{"example.com"})

			resp, err := sendFromGuest(t, proxyAddr, guestIP, "example.com", dns.TypeA)
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if resp.Rcode != dns.RcodeSuccess {
				t.Fatalf("Rcode = %d, want NOERROR", resp.Rcode)
			}
			if len(resp.Answer) != 0 {
				t.Errorf("%s not stripped from reply: %v", bad, resp.Answer)
			}
			if calls := recorder.snapshot(); len(calls) != 0 {
				t.Errorf("AllowIP called for %s: %+v", bad, calls)
			}
		})
	}
}

func TestProxyBlockedPrefixes(t *testing.T) {
	// A genuinely public, globally-routable address (Google DNS) that
	// IsPublicUnicast accepts — so only an operator-configured
	// BlockedPrefixes entry (how the manager passes a sandbox pool
	// placed outside RFC1918) can strip it. This exercises the config
	// layer on top of the built-in reserved-range filter. (CGNAT and
	// the other IANA special ranges are now caught by the built-in
	// filter regardless of config — see TestProxyStripsUnroutableAnswers.)
	upstream, _ := startStubUpstream(t, 60, "8.8.8.8")
	_, proxyAddr, recorder := startTestProxyCfg(t, upstream, []string{"example.com"}, func(c *Config) {
		c.BlockedPrefixes = []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24")}
	})

	resp, err := sendFromGuest(t, proxyAddr, guestIP, "example.com", dns.TypeA)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(resp.Answer) != 0 {
		t.Errorf("blocked-prefix address not stripped: %v", resp.Answer)
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Errorf("AllowIP called for blocked prefix: %+v", calls)
	}
}

// TestProxyBatchesAndCapsAnswerRecords covers the H3 fan-out fix:
// a fat reply produces exactly one AllowIP invocation carrying at
// most maxAnswerIPs records, and the guest sees the same capped
// set.
func TestProxyBatchesAndCapsAnswerRecords(t *testing.T) {
	ips := make([]string, 20)
	for i := range ips {
		ips[i] = fmt.Sprintf("93.184.216.%d", i+1)
	}
	upstream, _ := startStubUpstream(t, 60, ips...)
	_, proxyAddr, recorder := startTestProxy(t, upstream, []string{"example.com"})

	resp, err := sendFromGuest(t, proxyAddr, guestIP, "example.com", dns.TypeA)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(resp.Answer) != maxAnswerIPs {
		t.Errorf("guest sees %d answers, want cap %d", len(resp.Answer), maxAnswerIPs)
	}
	if got := recorder.batchCount(); got != 1 {
		t.Errorf("AllowIP invocations = %d, want 1 (batched per reply)", got)
	}
	if calls := recorder.snapshot(); len(calls) != maxAnswerIPs {
		t.Errorf("AllowIP records = %d, want cap %d", len(calls), maxAnswerIPs)
	}
}

func TestProxyRateLimitsPerSource(t *testing.T) {
	upstream, upstreamHits := startStubUpstream(t, 60, "93.184.216.34")
	_, proxyAddr, _ := startTestProxyCfg(t, upstream, []string{"example.com"}, func(c *Config) {
		c.SourceQPS = 5 // burst 10
	})

	// Fire 40 queries as fast as possible. The bucket admits the
	// burst (10) plus a small refill trickle; the rest must be
	// dropped before reaching upstream.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &dns.Client{
				Net:     "udp",
				Timeout: 300 * time.Millisecond,
				Dialer: &net.Dialer{
					LocalAddr: &net.UDPAddr{IP: net.ParseIP(guestIP), Port: 0},
					Timeout:   300 * time.Millisecond,
				},
			}
			req := new(dns.Msg)
			req.SetQuestion(dns.Fqdn("example.com"), dns.TypeA)
			_, _, _ = c.Exchange(req, proxyAddr)
		}()
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond) // let in-flight handlers settle

	hits := upstreamHits.Load()
	if hits == 0 {
		t.Fatal("all queries dropped — rate limiter over-aggressive")
	}
	if hits > 20 {
		t.Errorf("upstream hits = %d for 40 rapid queries with burst 10 — limiter not limiting", hits)
	}
}

func TestProxyBoundsInflightConcurrency(t *testing.T) {
	// Slow upstream so handlers hold their semaphore slot; with
	// MaxInflight=2, at most the first 2 of a 10-query burst do
	// real work — the rest are shed at the gate.
	upstream, upstreamHits := startStubUpstreamDelay(t, 500*time.Millisecond, 60, "93.184.216.34")
	_, proxyAddr, _ := startTestProxyCfg(t, upstream, []string{"example.com"}, func(c *Config) {
		c.MaxInflight = 2
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &dns.Client{
				Net:     "udp",
				Timeout: time.Second,
				Dialer: &net.Dialer{
					LocalAddr: &net.UDPAddr{IP: net.ParseIP(guestIP), Port: 0},
					Timeout:   time.Second,
				},
			}
			req := new(dns.Msg)
			req.SetQuestion(dns.Fqdn("example.com"), dns.TypeA)
			_, _, _ = c.Exchange(req, proxyAddr)
		}()
	}
	wg.Wait()

	hits := upstreamHits.Load()
	if hits == 0 {
		t.Fatal("all queries dropped — inflight gate over-aggressive")
	}
	if hits > 4 {
		t.Errorf("upstream hits = %d for a 10-query burst with MaxInflight=2", hits)
	}
}

// TestProxyCapsPerSourceInflight covers the R3 fix: even when the
// global pool has ample room, one source can't hold more than
// PerSourceInflight upstream round-trips at once — so a single guest
// pointed at a slow/stalling authoritative server can't monopolize
// the shared pool and starve other sandboxes' DNS.
func TestProxyCapsPerSourceInflight(t *testing.T) {
	// Slow upstream so handlers pile up. Global pool is 16 (would
	// admit all 10 queries), but PerSourceInflight=2 holds this one
	// source to ~2 concurrent — a couple of waves within the client
	// timeout, well under 10.
	upstream, upstreamHits := startStubUpstreamDelay(t, 500*time.Millisecond, 60, "93.184.216.34")
	_, proxyAddr, _ := startTestProxyCfg(t, upstream, []string{"example.com"}, func(c *Config) {
		c.MaxInflight = 16
		c.PerSourceInflight = 2
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &dns.Client{
				Net:     "udp",
				Timeout: time.Second,
				Dialer: &net.Dialer{
					LocalAddr: &net.UDPAddr{IP: net.ParseIP(guestIP), Port: 0},
					Timeout:   time.Second,
				},
			}
			req := new(dns.Msg)
			req.SetQuestion(dns.Fqdn("example.com"), dns.TypeA)
			_, _, _ = c.Exchange(req, proxyAddr)
		}()
	}
	wg.Wait()

	hits := upstreamHits.Load()
	if hits == 0 {
		t.Fatal("all queries dropped — per-source cap over-aggressive")
	}
	if hits > 4 {
		t.Errorf("upstream hits = %d for a single-source 10-query burst with PerSourceInflight=2 (global 16) — per-source cap not limiting", hits)
	}
}

// TestIsPublicUnicast pins the exact address ranges the egress filter
// treats as public (R1). Everything else — non-global-unicast,
// RFC1918, and the IANA special-purpose blocks — must be rejected so
// it reaches neither the guest nor the nftables allow-set.
func TestIsPublicUnicast(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// Public global-unicast — the only addresses that pass.
		{"8.8.8.8", true},
		{"93.184.216.34", true},
		{"1.1.1.1", true},
		// Non-global-unicast (netip rejects these).
		{"169.254.169.254", false}, // link-local cloud metadata
		{"127.0.0.1", false},       // loopback
		{"224.0.0.1", false},       // multicast
		{"255.255.255.255", false}, // broadcast
		{"0.0.0.0", false},         // unspecified
		// RFC1918 private.
		{"10.20.3.2", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		// IANA special-purpose ranges added by the R1 fix.
		{"100.100.100.200", false}, // Alibaba metadata (CGNAT)
		{"100.64.0.0", false},      // CGNAT low edge
		{"100.127.255.255", false}, // CGNAT high edge
		{"0.1.2.3", false},         // 0.0.0.0/8 non-zero host
		{"192.0.0.171", false},     // NAT64/DS-Lite
		{"192.0.2.1", false},       // TEST-NET-1
		{"192.88.99.1", false},     // 6to4 relay anycast
		{"198.18.0.1", false},      // benchmarking
		{"198.19.255.255", false},  // benchmarking (upper half of /15)
		{"198.51.100.1", false},    // TEST-NET-2
		{"203.0.113.1", false},     // TEST-NET-3
		{"240.0.0.1", false},       // reserved / future use
		// Just outside the special ranges — must stay public.
		{"100.63.255.255", true}, // one below CGNAT
		{"100.128.0.0", true},    // one above CGNAT
		{"198.17.255.255", true}, // one below benchmarking
		{"198.20.0.0", true},     // one above benchmarking
	}
	for _, c := range cases {
		if got := IsPublicUnicast(netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("IsPublicUnicast(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestInternalAnswer(t *testing.T) {
	vip := netip.MustParseAddr("10.20.255.254")
	allow := func(sandboxID, target string) bool { return target == "backend" }
	p := &Proxy{cfg: Config{InternalZone: "internal", InternalVIP: vip, InternalAuthz: allow}}

	// Authorized A query for an in-zone app → authoritative A record → the VIP.
	reqA := new(dns.Msg)
	reqA.SetQuestion("backend.internal.", dns.TypeA)
	m := p.internalAnswer(reqA, "sbx_1")
	if m == nil || !m.Authoritative || len(m.Answer) != 1 {
		t.Fatalf("A: want 1 authoritative answer, got %+v", m)
	}
	if a, ok := m.Answer[0].(*dns.A); !ok || a.A.String() != "10.20.255.254" {
		t.Errorf("A record = %v, want 10.20.255.254", m.Answer[0])
	}

	// Authorized AAAA → NODATA (non-nil NOERROR, empty answer) so a dual-stack
	// resolver falls back to the A record.
	reqAAAA := new(dns.Msg)
	reqAAAA.SetQuestion("backend.internal.", dns.TypeAAAA)
	if m := p.internalAnswer(reqAAAA, "sbx_1"); m == nil || len(m.Answer) != 0 || m.Rcode != dns.RcodeSuccess {
		t.Errorf("AAAA: want NODATA (empty NOERROR), got %+v", m)
	}

	// UNAUTHORIZED in-zone target → NXDOMAIN (no inventory leak), never an A.
	reqDeny := new(dns.Msg)
	reqDeny.SetQuestion("secret.internal.", dns.TypeA)
	if m := p.internalAnswer(reqDeny, "sbx_1"); m == nil || m.Rcode != dns.RcodeNameError || len(m.Answer) != 0 {
		t.Errorf("unauthorized: want NXDOMAIN, got %+v", m)
	}

	// Nil authorizer with the zone set → deny everything in-zone (fail closed).
	noAuthz := &Proxy{cfg: Config{InternalZone: "internal", InternalVIP: vip}}
	req := new(dns.Msg)
	req.SetQuestion("backend.internal.", dns.TypeA)
	if m := noAuthz.internalAnswer(req, "sbx_1"); m == nil || m.Rcode != dns.RcodeNameError {
		t.Errorf("nil authz: want NXDOMAIN (fail closed), got %+v", m)
	}

	// Out-of-zone, the bare zone, and a disabled zone → not ours (nil).
	for _, tc := range []struct {
		name string
		p    *Proxy
		q    string
	}{
		{"out-of-zone", p, "example.com."},
		{"bare-zone", p, "internal."},
		{"disabled", &Proxy{cfg: Config{}}, "backend.internal."},
	} {
		req := new(dns.Msg)
		req.SetQuestion(tc.q, dns.TypeA)
		if got := tc.p.internalAnswer(req, "sbx_1"); got != nil {
			t.Errorf("%s: internalAnswer = %+v, want nil", tc.name, got)
		}
	}
}
