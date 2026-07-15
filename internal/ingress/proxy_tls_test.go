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

// staticCerts is a CertProvider serving one keypair for every SNI.
type staticCerts struct{ cfg *tls.Config }

func (s staticCerts) TLSConfig() *tls.Config { return s.cfg }

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
