package ingress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

// loopbackVIP stands in for a real 10.21.x dummy-iface VIP in unit tests — the
// listener binding logic is identical. (freePort lives in wakeset_test.go.)
var loopbackVIP = netip.MustParseAddr("127.0.0.1")

// newL4Proxy builds a proxy fronting one running app "db" whose instance points at an
// echo backend, with a caller authorizer. Returns the proxy + the echo backend's port.
func newL4Proxy(t *testing.T, authz CallerAuthorizer) *Proxy {
	t.Helper()
	host, port, closeBackend := startEchoBackend(t)
	t.Cleanup(closeBackend)
	apps := fakeApps{apps: map[string]api.AppResponse{"db": runningApp("db", port, "sbx_db")}}
	inst := fakeInstances{ips: map[string]string{"sbx_db": host}}
	return New(Config{
		Resolver:      NewResolver(apps, inst, "", "internal", 0),
		InternalAuthz: authz,
		Logger:        quietLog(),
	})
}

func TestL4TCPSpliceToBackend(t *testing.T) {
	p := newL4Proxy(t, authzFunc(func(ip, target string) (string, bool) { return "caller", target == "db" }))
	defer p.Stop(context.Background())

	vipPort := freePort(t)
	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: vipPort, Proto: "tcp"}}); err != nil {
		t.Fatalf("AddInternalApp: %v", err)
	}

	// Connect to the VIP:port and prove bytes splice through to the app's backend.
	got := l4Roundtrip(t, fmt.Sprintf("127.0.0.1:%d", vipPort), "postgres-wire-bytes")
	if got != "postgres-wire-bytes" {
		t.Fatalf("splice mismatch: got %q", got)
	}
}

func TestL4AuthzDenyClosesWithoutReachingBackend(t *testing.T) {
	// Authorizer denies "db": the connection must be closed, no bytes echoed.
	p := newL4Proxy(t, authzFunc(func(ip, target string) (string, bool) { return "caller", false }))
	defer p.Stop(context.Background())

	vipPort := freePort(t)
	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: vipPort}}); err != nil { // default proto tcp
		t.Fatalf("AddInternalApp: %v", err)
	}

	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", vipPort), 2*time.Second)
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
	p := newL4Proxy(t, nil) // no authorizer → deny every internal call
	defer p.Stop(context.Background())

	vipPort := freePort(t)
	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: vipPort}}); err != nil {
		t.Fatalf("AddInternalApp: %v", err)
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", vipPort), 2*time.Second)
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
	p := newL4Proxy(t, authzFunc(func(ip, target string) (string, bool) { return "c", true }))
	defer p.Stop(context.Background())

	vipPort := freePort(t)
	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: vipPort}}); err != nil {
		t.Fatalf("AddInternalApp: %v", err)
	}
	// Works before removal.
	if got := l4Roundtrip(t, fmt.Sprintf("127.0.0.1:%d", vipPort), "x"); got != "x" {
		t.Fatalf("pre-remove splice: got %q", got)
	}
	p.RemoveInternalApp("db")

	// After removal the VIP:port no longer accepts (dials are refused).
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", vipPort), 200*time.Millisecond)
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
	p := newL4Proxy(t, authzFunc(func(ip, target string) (string, bool) { return "c", true }))
	defer p.Stop(context.Background())
	if err := p.AddInternalApp("db", loopbackVIP, []L4Port{{Port: freePort(t), Proto: "udp"}}); err == nil {
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
