package tlscert

import (
	"context"
	"testing"
	"time"
)

// A fresh terminate-mode domain with no cert and no failure is "pending"; a
// recorded ACME failure flips it to "failed" with the error; a later success
// clears the failure back to "pending" (until a cert is cached).
func TestStatusPendingFailedRecovery(t *testing.T) {
	p, err := New(Config{CertDir: t.TempDir()}) // manual mode is fine — we drive events directly
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)

	if got := p.Status("shop.example.com").State; got != "pending" {
		t.Fatalf("no cert, no failure → %q, want pending", got)
	}

	_ = p.onCertEvent(context.Background(), "cert_failed", map[string]any{
		"identifier": "shop.example.com",
		"error":      context.DeadlineExceeded,
	})
	st := p.Status("shop.example.com")
	if st.State != "failed" || st.LastError == "" || st.LastAttempt == nil {
		t.Fatalf("after cert_failed → %+v, want state=failed with error+attempt", st)
	}

	_ = p.onCertEvent(context.Background(), "cert_obtained", map[string]any{"identifier": "shop.example.com"})
	if got := p.Status("shop.example.com").State; got != "pending" {
		t.Fatalf("after cert_obtained (no cache yet) → %q, want pending (failure cleared)", got)
	}
}

// A domain served by a drop-in manual cert reports "manual" with the cert's
// expiry.
func TestStatusManual(t *testing.T) {
	certDir := t.TempDir()
	writeManualCert(t, certDir+"/manual", "shop", "shop.acme.com")
	p, err := New(Config{CertDir: certDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	st := p.Status("shop.acme.com")
	if st.State != "manual" || st.NotAfter == nil {
		t.Fatalf("manual domain → %+v, want state=manual with NotAfter", st)
	}
	if time.Until(*st.NotAfter) <= 0 {
		t.Errorf("manual NotAfter %v is not in the future", st.NotAfter)
	}
}
