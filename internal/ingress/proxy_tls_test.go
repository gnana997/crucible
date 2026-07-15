package ingress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

// selfSigned returns a cert+key for dnsName and a pool that trusts it.
func selfSigned(t *testing.T, dnsName string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	parsed, _ := x509.ParseCertificate(der)
	pool.AddCert(parsed)
	return cert, pool
}

// staticCerts is a CertProvider serving one keypair for every SNI, with no ACME
// (HTTP-01 always declines).
type staticCerts struct{ cfg *tls.Config }

func (s staticCerts) TLSConfig() *tls.Config                                      { return s.cfg }
func (s staticCerts) HandleHTTPChallenge(http.ResponseWriter, *http.Request) bool { return false }

// tlsClient makes an HTTP client whose TLS dials go to addr with the given SNI
// and trust pool, regardless of the request URL's host.
func tlsClient(addr, sni string, pool *x509.CertPool) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := &tls.Dialer{Config: &tls.Config{ServerName: sni, RootCAs: pool}}
			return d.DialContext(ctx, network, addr)
		},
	}}
}

// TestProxyTLSTerminates: with a cert source configured, a terminate-mode app's
// HTTPS is decrypted at the proxy and routed to the guest over plain HTTP.
func TestProxyTLSTerminates(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "served %s", r.Host)
	}))
	defer backend.Close()
	ip, port := backendAddr(t, backend.URL)

	apps := fakeApps{apps: map[string]api.AppResponse{"web": runningApp("web", port, "sbx_1")}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": ip}}
	cert, pool := selfSigned(t, "web.apps.local")

	p := New(Config{
		Resolver:  NewResolver(apps, inst, "apps.local", "", 0),
		TLSListen: "127.0.0.1:0",
		Certs:     staticCerts{&tls.Config{Certificates: []tls.Certificate{cert}}},
		Logger:    quietLog(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	client := tlsClient(p.tlsLn.Addr().String(), "web.apps.local", pool)
	resp, err := client.Get("https://web.apps.local/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if string(body) != "served web.apps.local" {
		t.Errorf("body = %q, want 'served web.apps.local' (terminated + routed)", body)
	}
}

// TestProxyTLSPassthroughWhenOptedOut: even with a cert source configured, a
// passthrough-mode app's raw TLS is piped to the guest, which presents its OWN
// cert (the client trusts only the guest cert, not the proxy's).
func TestProxyTLSPassthroughWhenOptedOut(t *testing.T) {
	// The "guest": a TLS server presenting the guest cert for raw.apps.local.
	guestCert, guestPool := selfSigned(t, "raw.apps.local")
	gln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guestSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "guest-served")
	})}
	go func() {
		_ = guestSrv.Serve(tls.NewListener(gln, &tls.Config{Certificates: []tls.Certificate{guestCert}}))
	}()
	defer func() { _ = guestSrv.Close() }()
	gip, gport, _ := net.SplitHostPort(gln.Addr().String())
	gportN, _ := parsePort(gport)

	raw := runningApp("raw", gportN, "sbx_r")
	raw.TLSMode = TLSModePassthrough // opt out of termination
	apps := fakeApps{apps: map[string]api.AppResponse{"raw": raw}}
	inst := fakeInstances{ips: map[string]string{"sbx_r": gip}}

	// The proxy has its OWN cert (for terminate-mode apps), but raw is passthrough.
	proxyCert, _ := selfSigned(t, "web.apps.local")
	p := New(Config{
		Resolver:  NewResolver(apps, inst, "apps.local", "", 0),
		TLSListen: "127.0.0.1:0",
		Certs:     staticCerts{&tls.Config{Certificates: []tls.Certificate{proxyCert}}},
		Logger:    quietLog(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop(context.Background())

	// Trusting only the GUEST cert must succeed → the guest's cert was presented,
	// proving the proxy piped rather than terminating with its own cert.
	client := tlsClient(p.tlsLn.Addr().String(), "raw.apps.local", guestPool)
	resp, err := client.Get("https://raw.apps.local/")
	if err != nil {
		t.Fatalf("passthrough request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "guest-served" {
		t.Fatalf("status=%d body=%q, want 200 'guest-served' (piped to the guest)", resp.StatusCode, body)
	}
}

// challengeCerts is a CertProvider that answers an ACME HTTP-01 challenge path
// (and has no usable tls.Config — the HTTP-01 test never terminates TLS).
type challengeCerts struct{ token string }

func (challengeCerts) TLSConfig() *tls.Config { return nil }
func (c challengeCerts) HandleHTTPChallenge(w http.ResponseWriter, r *http.Request) bool {
	if len(r.URL.Path) >= len(acmeChallengePrefix) && r.URL.Path[:len(acmeChallengePrefix)] == acmeChallengePrefix {
		_, _ = io.WriteString(w, c.token)
		return true
	}
	return false
}

const acmeChallengePrefix = "/.well-known/acme-challenge/"

// TestProxyHTTP01ChallengeShortCircuits: the :80 handler serves an ACME HTTP-01
// challenge via the cert provider before any app routing, and declines
// non-challenge requests (which then route/404 normally).
func TestProxyHTTP01ChallengeShortCircuits(t *testing.T) {
	apps := fakeApps{apps: map[string]api.AppResponse{"web": runningApp("web", 9, "sbx_1")}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": "127.0.0.1"}}
	p := New(Config{
		Resolver: NewResolver(apps, inst, "apps.local", "", 0),
		Certs:    challengeCerts{token: "tok-abc"},
		Logger:   quietLog(),
	})
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + acmeChallengePrefix + "xyz")
	if err != nil {
		t.Fatalf("challenge request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "tok-abc" {
		t.Errorf("challenge body = %q, want the token (HTTP-01 must short-circuit routing)", body)
	}

	// A non-challenge request for an unknown host is declined by the hook and 404s.
	req, _ := http.NewRequest(http.MethodGet, front.URL, nil)
	req.Host = "nope.apps.local"
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("normal request: %v", err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown host status = %d, want 404 (challenge hook must decline)", r2.StatusCode)
	}
}

// TestProxyHTTPSRedirect: a plaintext :80 request for a terminate-mode app is
// 301'd to https; an opted-out app (HTTPRedirect=false) and a passthrough app
// are served plain HTTP (routed, not redirected).
func TestProxyHTTPSRedirect(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "plain")
	}))
	defer backend.Close()
	ip, port := backendAddr(t, backend.URL)

	off := false
	term := runningApp("web", port, "sbx_1") // default terminate
	optOut := runningApp("plain", port, "sbx_2")
	optOut.HTTPRedirect = &off
	pass := runningApp("raw", port, "sbx_3")
	pass.TLSMode = TLSModePassthrough
	apps := fakeApps{apps: map[string]api.AppResponse{"web": term, "plain": optOut, "raw": pass}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": ip, "sbx_2": ip, "sbx_3": ip}}

	// certs != nil enables the redirect path; the provider is otherwise unused here.
	p := New(Config{
		Resolver: NewResolver(apps, inst, "apps.local", "", 0),
		Certs:    challengeCerts{},
		Logger:   quietLog(),
	})
	front := httptest.NewServer(p)
	defer front.Close()
	// Don't auto-follow redirects.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req := func(host string) *http.Response {
		r, _ := http.NewRequest(http.MethodGet, front.URL+"/p?x=1", nil)
		r.Host = host
		resp, err := client.Do(r)
		if err != nil {
			t.Fatalf("request %s: %v", host, err)
		}
		return resp
	}

	// terminate-mode → 301 to https, preserving path+query.
	resp := req("web.apps.local")
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("terminate app: status = %d, want 301", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://web.apps.local/p?x=1" {
		t.Errorf("Location = %q, want https://web.apps.local/p?x=1", loc)
	}
	_ = resp.Body.Close()

	// opted-out → served plain HTTP (200, routed).
	resp = req("plain.apps.local")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("opted-out app: status = %d, want 200 (no redirect)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// passthrough → served plain HTTP on :80 (the guest owns :443, no redirect).
	resp = req("raw.apps.local")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("passthrough app: status = %d, want 200 (no redirect)", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func parsePort(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad port %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
