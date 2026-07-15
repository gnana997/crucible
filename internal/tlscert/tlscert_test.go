package tlscert

import (
	"context"
	"testing"
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
