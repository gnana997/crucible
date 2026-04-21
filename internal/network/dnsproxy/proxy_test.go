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
// answers any A query with a canned record. Returns its address
// and a counter that tests can assert on to verify the proxy
// actually forwarded (vs. short-circuiting).
func startStubUpstream(t *testing.T, answerIP string, ttl uint32) (string, *atomic.Int64) {
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
			reply := new(dns.Msg)
			reply.SetReply(r)
			for _, q := range r.Question {
				if q.Qtype != dns.TypeA {
					continue
				}
				reply.Answer = append(reply.Answer, &dns.A{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    ttl,
					},
					A: net.ParseIP(answerIP).To4(),
				})
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
	recorder := newAllowIPRecorder()

	// bind :0 to get a free port
	listener, err := net.ListenPacket("udp", proxyIP+":0")
	if err != nil {
		t.Fatalf("proxy pre-bind: %v", err)
	}
	proxyAddr := listener.LocalAddr().String()
	_ = listener.Close() // release so Start can bind

	p, err := Start(Config{
		ListenAddr: proxyAddr,
		Upstream:   upstream,
		AllowIP:    recorder.record,
	})
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
	mu    sync.Mutex
	calls []allowIPCall
}

func newAllowIPRecorder() *allowIPRecorder { return &allowIPRecorder{} }

func (r *allowIPRecorder) record(_ context.Context, sandboxID string, ip netip.Addr, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, allowIPCall{SandboxID: sandboxID, IP: ip, TTL: ttl})
	return nil
}

func (r *allowIPRecorder) snapshot() []allowIPCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]allowIPCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// --- tests ------------------------------------------------------

func TestProxyAllowedQueryForwardsAndPokesNFT(t *testing.T) {
	upstream, upstreamHits := startStubUpstream(t, "93.184.216.34", 60)
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
	upstream, upstreamHits := startStubUpstream(t, "93.184.216.34", 60)
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
	upstream, _ := startStubUpstream(t, "93.184.216.34", 60)
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
	upstream, _ := startStubUpstream(t, "10.11.12.13", 30)
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
	upstream, _ := startStubUpstream(t, "1.2.3.4", 30)
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
		{Upstream: "1.1.1.1:53", AllowIP: func(context.Context, string, netip.Addr, time.Duration) error { return nil }},
		{ListenAddr: "127.0.0.2:0", AllowIP: func(context.Context, string, netip.Addr, time.Duration) error { return nil }},
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
		AllowIP:    func(context.Context, string, netip.Addr, time.Duration) error { return nil },
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
	upstream, _ := startStubUpstream(t, "1.2.3.4", 30)
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
