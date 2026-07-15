package tlscert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewRequiresCertDir(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New without CertDir should error")
	}
}

func TestACMENeedsAllow(t *testing.T) {
	// ACME (Email set) without a DecisionFunc gate would let any SNI burn a cert.
	if _, err := New(Config{CertDir: t.TempDir(), Email: "ops@example.com"}); err == nil {
		t.Fatal("ACME without Allow should error")
	}
}

func TestManualModeNoChallenge(t *testing.T) {
	p, err := New(Config{CertDir: t.TempDir()}) // no Email → manual-cert mode
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	if p.TLSConfig() == nil {
		t.Error("TLSConfig should be non-nil even in manual mode")
	}
	// Manual mode has no ACME issuer, so it never claims an HTTP-01 challenge.
	if p.HandleHTTPChallenge(nil, nil) {
		t.Error("manual mode must not handle HTTP-01 challenges")
	}
	// Prewarm is a no-op without ACME.
	if err := p.Prewarm(context.Background(), "x.example.com"); err != nil {
		t.Errorf("Prewarm (manual) = %v, want nil", err)
	}
}

func TestACMEModeBuilds(t *testing.T) {
	p, err := New(Config{
		CertDir: t.TempDir(),
		Email:   "ops@example.com",
		Staging: true,
		Allow:   func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New (ACME): %v", err)
	}
	t.Cleanup(p.Close)
	if p.TLSConfig() == nil {
		t.Error("TLSConfig should be non-nil in ACME mode")
	}
	if p.issuer == nil {
		t.Error("ACME mode should configure an issuer")
	}
}

// writeManualCert drops a self-signed <name>.crt + <name>.key into dir.
func writeManualCert(t *testing.T, dir, name, dnsName string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(filepath.Join(dir, name+".crt"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".key"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestManualCertLoaded(t *testing.T) {
	certDir := t.TempDir()
	writeManualCert(t, filepath.Join(certDir, "manual"), "shop", "shop.acme.com")
	// A stray .crt without a .key is ignored, not an error.
	if err := os.WriteFile(filepath.Join(certDir, "manual", "orphan.crt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := New(Config{CertDir: certDir}) // manual mode (no ACME)
	if err != nil {
		t.Fatalf("New with manual cert: %v", err)
	}
	t.Cleanup(p.Close)

	// The loaded cert is served for its SNI via the on-demand tls.Config.
	cert, err := p.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "shop.acme.com"})
	if err != nil || cert == nil {
		t.Fatalf("GetCertificate(shop.acme.com) = %v, %v; want the manual cert", cert, err)
	}
}
