package ingress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

// loopbackVIP stands in for a real 10.21.x dummy-iface VIP in unit tests — the
// listener binding logic is identical. (freePort lives in wakeset_test.go.)
var loopbackVIP = netip.MustParseAddr("127.0.0.1")

// startEchoAt runs a line-echo server bound to a specific addr (used to place the
// "guest" backend on 127.0.0.2:PORT so the VIP listener can bind 127.0.0.1:PORT — the
// L4 path dials the guest on the DECLARED internal port, which equals the VIP port).
func startEchoAt(t *testing.T, addr string) func() {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("echo listen %s: %v", addr, err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }(c)
		}
	}()
	return func() { _ = ln.Close() }
}

// newL4Proxy fronts app "db" that exposes NO HTTP --port (Port=0 — reachable ONLY
// app→app), whose single instance is an echo server at 127.0.0.2:P. Returns the proxy
// and P: the caller exposes db on --internal-port P and connects to 127.0.0.1(VIP):P.
// This is the realistic shape (a DB listens on its own port with no proxy --port) and
// pins the fix: the L4 dial uses the declared port, not the app's --port.
func newL4Proxy(t *testing.T, authz CallerAuthorizer) (*Proxy, int) {
	t.Helper()
	port := freePort(t)
	t.Cleanup(startEchoAt(t, fmt.Sprintf("127.0.0.2:%d", port)))
	apps := fakeApps{apps: map[string]api.AppResponse{"db": runningApp("db", 0, "sbx_db")}} // Port=0
	inst := fakeInstances{ips: map[string]string{"sbx_db": "127.0.0.2"}}
	p := New(Config{
		Resolver:      NewResolver(apps, inst, "", "internal", 0),
		InternalAuthz: authz,
		Logger:        quietLog(),
	})
	return p, port
}

func TestL4TCPSpliceToBackend(t *testing.T) {
	p, port := newL4Proxy(t, authzFunc(func(ip, target string) (string, bool) { return "caller", target == "db" }))
	defer p.Stop(context.Background())

	// db has NO --port; it is exposed only via --internal-port. The L4 dial MUST use
	// this declared port to reach the guest (regression: previously it used the app's
	// --port and no_route'd a DB with none).
	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: port, Proto: "tcp"}}); err != nil {
		t.Fatalf("AddInternalApp: %v", err)
	}
	got := l4Roundtrip(t, fmt.Sprintf("127.0.0.1:%d", port), "postgres-wire-bytes")
	if got != "postgres-wire-bytes" {
		t.Fatalf("splice mismatch: got %q", got)
	}
}

func TestL4AuthzDenyClosesWithoutReachingBackend(t *testing.T) {
	// Authorizer denies "db": the connection must be closed, no bytes echoed.
	p, port := newL4Proxy(t, authzFunc(func(ip, target string) (string, bool) { return "caller", false }))
	defer p.Stop(context.Background())

	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: port}}); err != nil { // default proto tcp
		t.Fatalf("AddInternalApp: %v", err)
	}

	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("hello"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	n, err := c.Read(buf)
	// Denied → the proxy closes its side; we get EOF (or 0 bytes), never an echo.
	if err == nil && n > 0 {
		t.Fatalf("denied caller got %d echoed bytes %q, want closed", n, buf[:n])
	}
}

func TestL4NilAuthzFailsClosed(t *testing.T) {
	p, port := newL4Proxy(t, nil) // no authorizer → deny every internal call
	defer p.Stop(context.Background())

	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: port}}); err != nil {
		t.Fatalf("AddInternalApp: %v", err)
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("hello"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	if n, err := c.Read(buf); err == nil && n > 0 {
		t.Fatalf("nil authz got %d echoed bytes, want closed", n)
	}
}

func TestL4RemoveAppClosesListener(t *testing.T) {
	p, port := newL4Proxy(t, authzFunc(func(ip, target string) (string, bool) { return "c", true }))
	defer p.Stop(context.Background())

	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: port}}); err != nil {
		t.Fatalf("AddInternalApp: %v", err)
	}
	// Works before removal.
	if got := l4Roundtrip(t, fmt.Sprintf("127.0.0.1:%d", port), "x"); got != "x" {
		t.Fatalf("pre-remove splice: got %q", got)
	}
	p.RemoveInternalApp("db")

	// After removal the VIP:port no longer accepts (dials are refused).
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err != nil {
			return // refused as expected
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("listener still accepting after RemoveInternalApp")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestL4RejectsUnknownProto(t *testing.T) {
	p, port := newL4Proxy(t, authzFunc(func(ip, target string) (string, bool) { return "c", true }))
	defer p.Stop(context.Background())
	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: port, Proto: "udp"}}); err == nil {
		t.Fatalf("expected error for unknown proto")
	}
}

func TestL4HTTPPortRoutesThroughL7(t *testing.T) {
	// An "http"-typed port hands the conn to the L7 server, which routes by
	// Host: <app>.internal and reverse-proxies to the app's instance.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "hello-http")
	}))
	defer backend.Close()
	bh, _, _ := net.SplitHostPort(backend.Listener.Addr().String())
	bport := backend.Listener.Addr().(*net.TCPAddr).Port

	apps := fakeApps{apps: map[string]api.AppResponse{"web": runningApp("web", bport, "sbx_web")}}
	inst := fakeInstances{ips: map[string]string{"sbx_web": bh}}
	p := New(Config{
		Resolver:      NewResolver(apps, inst, "", "internal", 0),
		InternalAuthz: authzFunc(func(ip, target string) (string, bool) { return "caller", target == "web" }),
		Logger:        quietLog(),
	})
	defer p.Stop(context.Background())

	vipPort := freePort(t)
	if err := p.AddInternalApp("web", loopbackVIP, []L4Port{{Port: vipPort, Proto: "http"}}); err != nil {
		t.Fatalf("AddInternalApp: %v", err)
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", vipPort), nil)
	req.Host = "web.internal" // the guest's app→app Host
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("http via L4 http port: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "hello-http" {
		t.Fatalf("L4 http route: status=%d body=%q, want 200 hello-http", resp.StatusCode, body)
	}
}

func TestL4MetricsHooks(t *testing.T) {
	dbPort := freePort(t)
	t.Cleanup(startEchoAt(t, fmt.Sprintf("127.0.0.2:%d", dbPort)))
	cachePort := freePort(t)
	apps := fakeApps{apps: map[string]api.AppResponse{
		"db":    runningApp("db", 0, "sbx_db"),
		"cache": runningApp("cache", 0, "sbx_cache"),
	}}
	inst := fakeInstances{ips: map[string]string{"sbx_db": "127.0.0.2", "sbx_cache": "127.0.0.2"}}

	var mu sync.Mutex
	outcomes := map[string]int{}
	var bytesTotal int64
	p := New(Config{
		Resolver:      NewResolver(apps, inst, "", "internal", 0),
		InternalAuthz: authzFunc(func(ip, target string) (string, bool) { return "c", target == "db" }),
		Logger:        quietLog(),
		OnL4Conn:      func(o string) { mu.Lock(); outcomes[o]++; mu.Unlock() },
		OnL4Bytes:     func(n int64) { mu.Lock(); bytesTotal += n; mu.Unlock() },
	})
	defer p.Stop(context.Background())

	// "db" is authorized (echo backend on 127.0.0.2:dbPort); "cache" is bound but the
	// authorizer denies it — a connection to it records the "denied" outcome.
	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: dbPort}}); err != nil {
		t.Fatalf("AddInternalApp db: %v", err)
	}
	if err := p.AddInternalApp("cache", loopbackVIP, []L4Port{{Port: cachePort}}); err != nil {
		t.Fatalf("AddInternalApp cache: %v", err)
	}

	// Denied connection: closed, and recorded as "denied".
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cachePort), 2*time.Second); err == nil {
		_, _ = c.Write([]byte("x"))
		_ = c.Close()
	}

	// A spliced roundtrip: expect outcome "spliced" and bytes counted (both directions).
	if got := l4Roundtrip(t, fmt.Sprintf("127.0.0.1:%d", dbPort), "hello"); got != "hello" {
		t.Fatalf("splice: got %q", got)
	}

	// Wait for the deferred bytes report (fires on conn close).
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		spliced, denied, b := outcomes["spliced"], outcomes["denied"], bytesTotal
		mu.Unlock()
		if spliced >= 1 && denied >= 1 && b >= int64(len("hello")*2) { // echoed both ways
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics not recorded: outcomes=%v bytes=%d (want spliced>=1, denied>=1, bytes>=10)", outcomes, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestL4PerAppCapShedsAndIsolatesApps(t *testing.T) {
	p := New(Config{Logger: quietLog()})
	defer p.Stop(context.Background())
	m := p.l4

	// Fill "db" to its per-app cap.
	for i := 0; i < maxPerAppL4Conns; i++ {
		if !m.acquire("db") {
			t.Fatalf("acquire %d should succeed (under per-app cap)", i)
		}
	}
	// The next db connection is shed — one app can't exceed its share.
	if m.acquire("db") {
		t.Fatal("acquire past the per-app cap should be shed")
	}
	// A DIFFERENT app is unaffected by db saturating its cap (the fairness fix).
	if !m.acquire("other") {
		t.Fatal("a different app must still acquire while db is capped")
	}
	// Releasing one db slot frees exactly one.
	m.release("db")
	if !m.acquire("db") {
		t.Fatal("acquire after release should succeed")
	}
	if m.acquire("db") {
		t.Fatal("db should be capped again after re-filling the one freed slot")
	}
}

func TestL4TokenBucketBurstThenDeny(t *testing.T) {
	tb := &tokenBucket{rate: 1, burst: 3, tokens: 3}
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !tb.allow(now) {
			t.Fatalf("burst token %d should be allowed", i)
		}
	}
	if tb.allow(now) {
		t.Fatal("a 4th connection within the burst window must be denied")
	}
	// 2 seconds later at 1 token/s, 2 tokens have refilled.
	if !tb.allow(now.Add(2 * time.Second)) {
		t.Fatal("a refilled token should be allowed")
	}
}

func TestL4SlotConnReleasesExactlyOnce(t *testing.T) {
	p := New(Config{Logger: quietLog()})
	defer p.Stop(context.Background())
	m := p.l4
	if !m.acquire("db") {
		t.Fatal("acquire")
	}
	c1, c2 := net.Pipe()
	defer func() { _ = c2.Close() }()
	released := 0
	sc := &slotConn{Conn: c1, release: func() { m.release("db"); released++ }}
	_ = sc.Close()
	_ = sc.Close() // idempotent — must not double-release the slot
	if released != 1 {
		t.Fatalf("slot released %d times, want exactly 1", released)
	}
	m.inflMu.Lock()
	n := m.infl["db"]
	m.inflMu.Unlock()
	if n != 0 {
		t.Fatalf("infl[db] = %d after release, want 0", n)
	}
}

// l4Roundtrip opens a fresh TCP conn to addr, writes payload, and returns what the
// spliced backend echoes back.
func l4Roundtrip(t *testing.T, addr, payload string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	return string(buf)
}
