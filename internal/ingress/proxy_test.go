package ingress

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// backendAddr splits an httptest.Server URL into (ip, port).
func backendAddr(t *testing.T, url string) (string, int) {
	t.Helper()
	host := url[len("http://"):]
	ip, portStr, err := net.SplitHostPort(host)
	if err != nil {
		t.Fatalf("split %q: %v", host, err)
	}
	port, _ := strconv.Atoi(portStr)
	return ip, port
}

func TestProxyHTTPRoutesByHost(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "served %s", r.Host)
	}))
	defer backend.Close()
	ip, port := backendAddr(t, backend.URL)

	apps := fakeApps{apps: map[string]api.AppResponse{"web": runningApp("web", port, "sbx_1")}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": ip}}
	p := New(Config{Resolver: NewResolver(apps, inst, "apps.local", 0), Logger: quietLog()})

	front := httptest.NewServer(p) // *Proxy is an http.Handler
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL, nil)
	req.Host = "web.apps.local"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	// The proxy preserves the app's Host header on the way to the backend.
	if string(body) != "served web.apps.local" {
		t.Errorf("body = %q, want 'served web.apps.local'", body)
	}
}

func TestProxyHTTPStatusForBadRoutes(t *testing.T) {
	// Unknown app → 404; known app with no ready instance → 502.
	apps := fakeApps{apps: map[string]api.AppResponse{
		"web": runningApp("web", 80, ""), // no instance
	}}
	p := New(Config{Resolver: NewResolver(apps, fakeInstances{ips: map[string]string{}}, "apps.local", 0), Logger: quietLog()})
	front := httptest.NewServer(p)
	defer front.Close()

	for host, want := range map[string]int{
		"nope.apps.local": http.StatusNotFound,   // unknown app
		"web.apps.local":  http.StatusBadGateway, // no ready instance
		"web.example.com": http.StatusNotFound,   // off-domain
	} {
		req, _ := http.NewRequest(http.MethodGet, front.URL, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %s: %v", host, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("host %s: status %d, want %d", host, resp.StatusCode, want)
		}
	}
}

func TestPeekSNI(t *testing.T) {
	c, s := net.Pipe()
	go func() {
		// A real ClientHello carrying SNI. The handshake never completes (we
		// don't answer), which is fine — we only want the ClientHello.
		_ = tls.Client(c, &tls.Config{ServerName: "web.apps.local", InsecureSkipVerify: true}).
			HandshakeContext(context.Background())
		_ = c.Close()
	}()

	sni, hello, err := peekSNI(s, 2*time.Second)
	if err != nil {
		t.Fatalf("peekSNI: %v", err)
	}
	if sni != "web.apps.local" {
		t.Errorf("sni = %q, want web.apps.local", sni)
	}
	if len(hello) == 0 {
		t.Error("no ClientHello bytes captured for replay")
	}
}
