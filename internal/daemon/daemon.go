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
	"time"

	"github.com/gnana997/crucible/internal/sandbox"
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
	s.http = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.logRequests(s.routes()),
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

// ListenAndServe binds the configured address and serves until the server
// is shut down or an error occurs. Returns http.ErrServerClosed on clean
// shutdown (which callers typically treat as non-error).
func (s *Server) ListenAndServe() error {
	s.cfg.Logger.Info("crucible daemon listening", "addr", s.cfg.Addr)
	return s.http.ListenAndServe()
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
	return nil
}
