// Package tlscert wraps CertMagic to give the ingress proxy automatic HTTPS for
// app domains: on-demand ACME issuance (Let's Encrypt) gated to registered app
// domains, TLS-ALPN-01 + HTTP-01 challenges, background renewal, and FileStorage
// under a cert directory. The daemon stays provider-agnostic otherwise — this is
// the one place cloud-CA plumbing lives, kept out of internal/ingress.
package tlscert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

// Config configures the provider.
type Config struct {
	// CertDir is the FileStorage root for certs/keys/ACME state (required).
	CertDir string
	// Email is the ACME account email. Empty disables ACME (manual certs from
	// CertDir only — see LoadManual).
	Email string
	// CAURL overrides the ACME directory URL (e.g. a Pebble/private-CA endpoint).
	// Empty uses Let's Encrypt (production, or staging when Staging is true).
	CAURL string
	// Staging selects the Let's Encrypt staging CA (higher rate limits, untrusted
	// certs) when CAURL is empty. Ignored when CAURL is set.
	Staging bool
	// CARootPEM, when non-empty, is a PEM bundle of root CA(s) to trust when
	// connecting to the ACME server — for a private or test CA (e.g. Pebble,
	// step-ca) whose endpoint isn't signed by a public root. Empty uses the
	// system roots. Does not affect image pulls or other daemon TLS.
	CARootPEM []byte
	// Allow gates on-demand issuance: it must return true only for domains that
	// map to a real, terminate-mode app, so a stray SNI can never trigger a cert
	// (abuse / rate-limit guard). Required when Email is set.
	Allow func(domain string) bool
}

// Provider satisfies ingress.CertProvider (TLSConfig + HandleHTTPChallenge).
type Provider struct {
	cache  *certmagic.Cache
	magic  *certmagic.Config
	issuer *certmagic.ACMEIssuer // nil when Email == "" (manual-cert-only)
	tlsCfg *tls.Config           // built once; on-demand GetCertificate + ALPN protos
}

// New builds a provider. With Email set it enables ACME/on-demand; without, it
// only serves certs already present in CertDir (manual mode).
func New(c Config) (*Provider, error) {
	if c.CertDir == "" {
		return nil, fmt.Errorf("tlscert: CertDir is required")
	}
	if c.Email != "" && c.Allow == nil {
		return nil, fmt.Errorf("tlscert: Allow is required when Email (ACME) is set")
	}

	p := &Provider{}
	p.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return p.magic, nil
		},
	})
	cache := p.cache
	tmpl := certmagic.Config{
		Storage: &certmagic.FileStorage{Path: c.CertDir},
		Logger:  zap.NewNop(), // certmagic is chatty by default; the daemon logs its own summary
	}
	if c.Email != "" {
		tmpl.OnDemand = &certmagic.OnDemandConfig{
			DecisionFunc: func(_ context.Context, name string) error {
				if c.Allow(name) {
					return nil
				}
				return fmt.Errorf("tlscert: issuance denied for %q (not a registered app domain)", name)
			},
		}
	}
	p.magic = certmagic.New(cache, tmpl)

	if c.Email != "" {
		ca := c.CAURL
		if ca == "" {
			ca = certmagic.LetsEncryptProductionCA
			if c.Staging {
				ca = certmagic.LetsEncryptStagingCA
			}
		}
		issTmpl := certmagic.ACMEIssuer{
			CA:     ca,
			Email:  c.Email,
			Agreed: true,
		}
		if len(c.CARootPEM) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(c.CARootPEM) {
				return nil, fmt.Errorf("tlscert: CARootPEM contains no valid certificates")
			}
			issTmpl.TrustedRoots = pool
		}
		p.issuer = certmagic.NewACMEIssuer(p.magic, issTmpl)
		p.magic.Issuers = []certmagic.Issuer{p.issuer}
	}

	// Manual certs: an operator can drop `<name>.crt` + `<name>.key` pairs into
	// <CertDir>/manual/ to serve their own certificate for a domain (no ACME).
	// Loaded unmanaged, so certmagic serves them by SNI but never renews them.
	if _, err := p.loadManualCerts(filepath.Join(c.CertDir, "manual")); err != nil {
		return nil, err
	}

	// Build the serving tls.Config once. certmagic's TLSConfig advertises only
	// "acme-tls/1" (the TLS-ALPN-01 challenge); add "http/1.1" so a real client
	// (which offers h2/http1.1) can negotiate ALPN. h2 is omitted: the proxy
	// feeds terminated conns to a plain http.Server not configured for HTTP/2.
	p.tlsCfg = p.magic.TLSConfig()
	if !slices.Contains(p.tlsCfg.NextProtos, "http/1.1") {
		p.tlsCfg.NextProtos = append(p.tlsCfg.NextProtos, "http/1.1")
	}
	return p, nil
}

// loadManualCerts loads every <name>.crt + <name>.key pair in dir as an
// unmanaged certificate. A missing dir is fine (returns 0). Returns how many
// pairs loaded.
func (p *Provider) loadManualCerts(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("tlscert: read manual dir %s: %w", dir, err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".crt") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".crt")
		crt := filepath.Join(dir, e.Name())
		key := filepath.Join(dir, base+".key")
		if _, err := os.Stat(key); err != nil {
			continue // no matching key — skip
		}
		if _, err := p.magic.CacheUnmanagedCertificatePEMFile(context.Background(), crt, key, nil); err != nil {
			return n, fmt.Errorf("tlscert: load manual cert %s: %w", crt, err)
		}
		n++
	}
	return n, nil
}

// NotAfter returns the expiry of a managed cert for domain, if one is cached.
func (p *Provider) NotAfter(domain string) (time.Time, bool) {
	cert, err := p.magic.CacheManagedCertificate(context.Background(), domain)
	if err != nil || cert.Leaf == nil {
		return time.Time{}, false
	}
	return cert.Leaf.NotAfter, true
}

// TLSConfig returns the serving *tls.Config: GetCertificate loads or obtains the
// cert for the handshake SNI on-demand, handles TLS-ALPN-01 challenges, and
// advertises http/1.1 (plus acme-tls/1) for ALPN.
func (p *Provider) TLSConfig() *tls.Config { return p.tlsCfg }

// HandleHTTPChallenge serves an ACME HTTP-01 challenge, returning true when the
// request was a challenge (and was handled). False for a normal request, or when
// ACME is disabled (manual-cert mode).
func (p *Provider) HandleHTTPChallenge(w http.ResponseWriter, r *http.Request) bool {
	if p.issuer == nil {
		return false
	}
	return p.issuer.HandleHTTPChallenge(w, r)
}

// Prewarm obtains (or loads) a cert for domain now, so the first live request
// isn't delayed by issuance. Best-effort: callers log and continue on error,
// since on-demand still covers it on the first handshake. No-op without ACME.
func (p *Provider) Prewarm(ctx context.Context, domain string) error {
	if p.issuer == nil {
		return nil
	}
	return p.magic.ManageAsync(ctx, []string{domain})
}

// Close stops the certificate cache's maintenance goroutine.
func (p *Provider) Close() {
	if p.cache != nil {
		p.cache.Stop()
	}
}
