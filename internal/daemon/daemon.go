// Package daemon is the HTTP surface of crucible: it translates REST
// calls into operations on a sandbox.Manager.
//
// The daemon is deliberately thin — it parses JSON, validates inputs,
// calls the Manager, and maps errors to HTTP status codes. Anything
// business-logic-shaped lives in the sandbox package.
//
// Routes (wk1):
//
//	GET    /healthz               liveness probe
//	POST   /sandboxes             create a sandbox, body optional
//	GET    /sandboxes             list all active sandboxes
//	GET    /sandboxes/{id}        details for a single sandbox
//	DELETE /sandboxes/{id}        shut down and remove
//
// All 2xx responses are `application/json`; errors use
// `{"error": "..."}` with a 4xx or 5xx status.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/internal/logstore"
	"github.com/gnana997/crucible/internal/metrics"
	"github.com/gnana997/crucible/internal/registryauth"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/internal/secretstore"
	"github.com/gnana997/crucible/internal/tokenstore"
	"github.com/gnana997/crucible/internal/volume"
	"github.com/gnana997/crucible/sdk/api"
)

// Server hosts the crucible HTTP API.
//
// Build with New; drive with ListenAndServe (or Serve for tests).
// Shutdown gracefully stops the listener and waits for in-flight
// requests. The Manager and Logger are not owned by the Server — the
// caller is responsible for their lifecycle.
type Server struct {
	cfg  Config
	http *http.Server

	// bgCtx bounds background goroutines (the per-sandbox log drains).
	// Shutdown cancels it and then waits on bgWG so no drain outlives
	// the server.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
}

// Config wires a Server to its dependencies.
type Config struct {
	// Manager is required. All request handlers dispatch through it.
	Manager *sandbox.Manager

	// Addr is the TCP listen address (e.g. "127.0.0.1:7878"). Required.
	Addr string

	// Logger receives request-level log lines. Nil means slog.Default.
	Logger *slog.Logger

	// ReadTimeout, WriteTimeout, IdleTimeout default to sensible values
	// if zero. WriteTimeout needs to cover the slowest sandbox boot; 60s
	// is the floor for real Firecracker boots.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// Metrics, when non-nil, is served at GET /metrics. Nil disables the
	// endpoint (the route returns 404), which is the default in tests.
	Metrics *metrics.Metrics

	// TokenStore, when non-nil and holding at least one key, requires every
	// request (except /healthz) to present a valid `Authorization: Bearer
	// <key>`. Nil or empty means no auth — the loopback-only default.
	TokenStore *tokenstore.Store

	// TLSCert/TLSKey, when set, make ListenAndServe serve HTTPS. Required
	// when binding a non-loopback address (the caller enforces that).
	TLSCert string
	TLSKey  string

	// Images, when non-nil, enables the /images routes (OCI pull,
	// import, list, delete). Nil makes those routes answer 501.
	Images ImageStore

	// LogStore, when non-nil, enables durable per-sandbox logs: a
	// background drain of each service's output into the store, exec
	// activity capture, and the GET /sandboxes/{id}/logs route. Nil
	// makes that route answer 501 and skips the drain/capture.
	LogStore *logstore.Store

	// AppManager, when non-nil, enables the /apps routes: durable apps
	// the daemon reconciles into running instances (v0.4 durability).
	// Nil makes those routes answer 501.
	AppManager *app.Manager

	// CertStatusSource, when set, returns the managed-cert status for a domain —
	// wired from the TLS provider so `GET /apps/{name}/domains?detail=1` can
	// report per-domain certificate state. Nil ⇒ no cert managed (passthrough or
	// termination disabled): terminate-mode domains report "pending".
	CertStatusSource func(domain string) api.CertStatus
	// ProxyDomain is the ingress base domain, so a detailed domains view can
	// include the app's generated <app>.<proxy-domain> name. Empty ⇒ omit it.
	ProxyDomain string

	// RegistryStore, when non-nil, enables the /registry/credentials routes
	// (manage private-registry pull credentials) and feeds those credentials
	// to image pulls. Nil makes those routes answer 501 and pulls stay
	// anonymous.
	RegistryStore *registryauth.Store

	// SecretStore, when non-nil, enables the /secrets routes (manage encrypted
	// secret bundles) and injects bound bundles into an app's env at boot. Nil
	// (no master key configured) makes those routes answer 501 and SecretEnvFrom
	// is rejected.
	SecretStore *secretstore.Store

	// Volumes, when non-nil, enables the /volumes routes (create/list/delete
	// persistent volumes) and reflects them in create requests. Nil makes
	// those routes answer 501 and a create requesting a volume is rejected.
	Volumes *volume.Manager

	// ReloadVolumeKeys re-reads the configured volume encryption key sources and
	// swaps the keyring in without a restart. nil when encryption is off; the
	// /volumes/keys/reload route answers 501 then.
	ReloadVolumeKeys func() error
}

const (
	defaultReadTimeout  = 30 * time.Second
	defaultWriteTimeout = 60 * time.Second
	defaultIdleTimeout  = 120 * time.Second
	maxRequestBody      = 1 << 20 // 1 MiB cap on POST bodies
)

// New constructs a Server. It validates config and wires routes but does
// not bind the listener — call ListenAndServe for that.
func New(cfg Config) (*Server, error) {
	if cfg.Manager == nil {
		return nil, errors.New("daemon: Config.Manager is required")
	}
	if cfg.Addr == "" {
		return nil, errors.New("daemon: Config.Addr is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}

	s := &Server{cfg: cfg}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.http = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.logRequests(s.auth(s.enforcePolicy(s.routes()))),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}
	return s, nil
}

// Handler returns the raw http.Handler with middleware applied. Useful
// for tests that want to use httptest.NewServer rather than binding a
// real TCP port.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// SetAppManager wires the durable-app control plane after construction.
// Two-phase because the app manager's instantiator needs this Server (for
// buildCreateConfig + the sandbox Manager), while the Server's routes need
// the app manager — so the caller builds the Server, then the app manager
// from s.NewAppInstantiator(), then calls this before serving.
func (s *Server) SetAppManager(m *app.Manager) { s.cfg.AppManager = m }

// SetCertStatusSource wires the TLS provider's per-domain cert status and the
// ingress base domain (for the app's generated name) into the detailed domains
// view. Call before serving; nil source leaves terminate-mode domains "pending".
func (s *Server) SetCertStatusSource(fn func(domain string) api.CertStatus, proxyDomain string) {
	s.cfg.CertStatusSource = fn
	s.cfg.ProxyDomain = proxyDomain
}

// ListenAndServe binds the configured address and serves until the server
// is shut down or an error occurs. Returns http.ErrServerClosed on clean
// shutdown (which callers typically treat as non-error).
func (s *Server) ListenAndServe() error {
	if s.cfg.TLSCert != "" {
		s.cfg.Logger.Info("crucible daemon listening (TLS)", "addr", s.cfg.Addr)
		return s.http.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
	}
	s.cfg.Logger.Info("crucible daemon listening", "addr", s.cfg.Addr)
	return s.http.ListenAndServe()
}

// auth enforces bearer-token auth whenever the token store holds any keys.
// /healthz is always exempt so liveness probes work without a credential. On a
// valid key it attaches the key's policy (nil for an unscoped key) to the
// request context, and rejects an expired key — both via VerifyPolicy.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store := s.cfg.TokenStore
		if store == nil || r.URL.Path == "/healthz" || !store.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		id, ok := store.Identify(bearerToken(r.Header.Get("Authorization")))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="crucible"`)
			writeError(w, http.StatusUnauthorized, errors.New("missing or invalid API key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}

// bearerToken extracts the credential from an "Authorization: Bearer <key>"
// header, or returns "" if the header is absent or a different scheme.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

// Serve serves requests on the already-bound listener. Parallels
// http.Server.Serve; used in tests with net.Listen("tcp", "127.0.0.1:0").
func (s *Server) Serve(ln net.Listener) error {
	s.cfg.Logger.Info("crucible daemon serving", "addr", ln.Addr().String())
	return s.http.Serve(ln)
}

// Shutdown gracefully stops the server: stops accepting new connections,
// waits for in-flight requests up to ctx's deadline, then returns.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cfg.Logger.Info("crucible daemon shutting down")
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("daemon: http shutdown: %w", err)
	}
	// Stop the background log drains and wait for them to exit.
	if s.bgCancel != nil {
		s.bgCancel()
	}
	s.bgWG.Wait()
	return nil
}
