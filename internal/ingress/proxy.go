package ingress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/reuseport"
	"github.com/gnana997/crucible/internal/telemetry"
)

const (
	dialTimeout       = 5 * time.Second
	sniPeekTimeout    = 10 * time.Second
	httpHeaderTimeout = 10 * time.Second // Slowloris guard on the HTTP listener
	proxyIdleTimeout  = 5 * time.Minute  // close a spliced/keep-alive conn idle this long
	maxTLSConns       = 1024             // cap concurrent SNI-passthrough conns
)

type targetKey struct{}

// CallerAuthorizer authorizes an app→app (internal-zone) request: given the
// source guest address and the target app name, is the call allowed? It returns
// the resolved caller app name for logging. Satisfied by the daemon, which maps
// the source IP to the calling app and checks its can_call grant.
type CallerAuthorizer interface {
	AuthorizeCall(callerIP, targetApp string) (callerApp string, allowed bool)
}

// Config configures the ingress proxy.
type Config struct {
	Resolver   *Resolver
	HTTPListen string // e.g. ":80"; empty disables the HTTP (host-header) proxy
	TLSListen  string // e.g. ":443"; empty disables the TLS (SNI-passthrough) proxy
	Logger     *slog.Logger

	// InternalListen is the address the app→app (backend.internal) listener binds
	// — the DNS anycast VIP + internal port (e.g. "10.20.255.254:80"), reachable
	// only from guest netns. Empty disables app→app networking. Host routing is
	// identical to HTTPListen but over the internal zone; wake-on-request applies,
	// so an internal call wakes a scaled-to-zero callee.
	InternalListen string

	// InternalAuthz authorizes each app→app request (default-deny). When
	// InternalListen is set this MUST be set too: a nil authorizer denies every
	// internal request (fail closed), so an unauthorized call never even wakes the
	// callee.
	InternalAuthz CallerAuthorizer

	// Waker, when set, makes the proxy wake a slept app on the first request
	// for it (scale-to-zero): an ErrAsleep resolve triggers a coalesced wake and
	// the request is held until the app is running. Nil disables wake-on-request
	// (a slept app then 503s).
	Waker Waker

	// Activity, when set, records per-app request activity (for the idle
	// monitor that auto-sleeps idle apps). Nil disables activity tracking.
	Activity *ActivityTracker

	// OnWake, when set, is called with the proxy-observed wake latency each time
	// a slept app is woken on request (for the wake-latency metric).
	OnWake func(latency time.Duration)

	// OnInternal, when set, is called once per authorized app→app request (for
	// the internal-request metric).
	OnInternal func()

	// OnRequest, when set, is called once per routed request to a KNOWN app with
	// the resolved app name, HTTP status class ("2xx"…"5xx"), latency, and whether
	// it arrived on the internal (app→app) listener. Requests for unknown apps
	// (404) are not reported, so metric label cardinality stays bounded.
	OnRequest func(app, code string, latency time.Duration, internal bool)
}

// Proxy is the daemon-owned ingress front door: :80 host-header routing (L7,
// via httputil.ReverseProxy) and :443 SNI passthrough (L4, no TLS termination —
// the guest owns its cert), both routed to an app's current instance via the
// Resolver. In-process, mirroring the DNS proxy.
type Proxy struct {
	resolver       *Resolver
	log            *slog.Logger
	httpListen     string
	tlsListen      string
	internalListen string

	authz       CallerAuthorizer // app→app authorization; nil = deny internal
	balancer    *Balancer        // picks an instance from an app's endpoint set
	rp          *httputil.ReverseProxy
	httpSrv     *http.Server
	internalSrv *http.Server
	tlsLn       net.Listener
	tlsSem      chan struct{}    // bounds concurrent SNI-passthrough handlers
	coord       *wakeCoordinator // nil when no Waker configured
	activity    *ActivityTracker // nil when activity tracking disabled
	onWake      func(time.Duration)
	onInternal  func()
	onRequest   func(app, code string, latency time.Duration, internal bool)
	wg          sync.WaitGroup
}

// New builds a proxy from cfg. Call Start to bind and serve.
func New(cfg Config) *Proxy {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	p := &Proxy{
		resolver:       cfg.Resolver,
		log:            log,
		httpListen:     cfg.HTTPListen,
		tlsListen:      cfg.TLSListen,
		internalListen: cfg.InternalListen,
		authz:          cfg.InternalAuthz,
		balancer:       NewBalancer(),
		tlsSem:         make(chan struct{}, maxTLSConns),
	}
	if cfg.Waker != nil {
		p.coord = newWakeCoordinator(cfg.Waker, 0)
	}
	p.activity = cfg.Activity
	p.onWake = cfg.OnWake
	p.onInternal = cfg.OnInternal
	p.onRequest = cfg.OnRequest
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Always set by ServeHTTP before this runs; comma-ok so a missing
			// value degrades to a dial failure (→ 502) instead of a panic.
			tg, _ := pr.In.Context().Value(targetKey{}).(Target)
			pr.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort(tg.GuestIP, strconv.Itoa(tg.Port))})
			pr.Out.Host = pr.In.Host // preserve the app's Host header
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Feed passive outlier detection: a dial/upstream failure counts
			// against the instance we picked, ejecting it after repeated failures.
			if tg, ok := r.Context().Value(targetKey{}).(Target); ok {
				p.balancer.Fail(tg.InstanceID)
			}
			p.log.Warn("ingress: upstream error", "host", r.Host, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p
}

// ServeHTTP is the external (proxy-domain) L7 handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.handle(w, r, false)
}

// handle resolves the app from the request Host and reverse-proxies to its
// current instance. internal selects the app→app zone (backend.internal) over
// the external proxy domain; the two share the ReverseProxy, wake path, and
// activity tracking. Unknown host → 404; app with no ready instance → 502.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request, internal bool) {
	resolveSet, appName := p.resolver.ResolveSet, p.resolver.AppName
	if internal {
		resolveSet, appName = p.resolver.ResolveSetInternal, p.resolver.AppNameInternal
	}
	name := appName(r.Host)

	// Per-app request metric (v0.5.4): capture the final status + latency and
	// report once on return, but ONLY for a KNOWN app — an unknown/unauthorized
	// Host never counts, so label cardinality stays bounded to real apps. The
	// wrapper delegates Flush/Hijack so streaming and websocket upgrades still work.
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	w = sw
	known := false
	defer func() {
		if p.onRequest != nil && known && name != "" {
			p.onRequest(name, telemetry.StatusClass(sw.status), time.Since(start), internal)
		}
	}()

	if internal {
		// Authorize BEFORE resolve/wake so an unauthorized call never wakes the
		// callee. A denied or unknown caller gets 403; an out-of-zone host, 404.
		// Neither counts (known stays false) — the caller isn't a known target.
		if !p.authorizeInternal(w, r, name) {
			return
		}
		if p.onInternal != nil {
			p.onInternal()
		}
	}
	set, err := resolveSet(r.Host)
	if errors.Is(err, ErrAsleep) && p.coord != nil {
		// Slept app: wake it (coalesced across a herd) and re-resolve, holding
		// this request in the blocked goroutine until it is running.
		set, err = p.wakeAndResolve(r.Context(), r.Host, internal)
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrAsleep):
			// Wake failed or timed out (or no waker configured): clean 503.
			known = true
			http.Error(w, "app is asleep", http.StatusServiceUnavailable)
		case errors.Is(err, ErrNoInstance):
			known = true
			http.Error(w, "app has no ready instance", http.StatusBadGateway)
		default:
			// Unknown app: 404, uncounted (bounded cardinality).
			http.Error(w, "no such app", http.StatusNotFound)
		}
		return
	}
	known = true
	// Balance across the app's endpoint set (P2C least-request); release the
	// in-flight count when the request completes.
	tg, release := p.balancer.Pick(set)
	defer release()
	if p.activity != nil {
		p.activity.begin(name)
		defer p.activity.end(name)
	}
	r = r.WithContext(context.WithValue(r.Context(), targetKey{}, tg))
	p.rp.ServeHTTP(w, r)
}

// statusWriter wraps an http.ResponseWriter to capture the final status code for
// the per-app request metric, while delegating Flush and Hijack so streaming
// responses and websocket upgrades through the reverse proxy keep working.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.written {
		s.status = http.StatusOK
		s.written = true
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// authorizeInternal enforces app→app default-deny for an internal request. It
// returns false (and writes the response) when the target is out of zone (404),
// or the caller is unidentified/unauthorized, or no authorizer is configured
// (403 — fail closed).
func (p *Proxy) authorizeInternal(w http.ResponseWriter, r *http.Request, target string) bool {
	if target == "" {
		http.Error(w, "no such app", http.StatusNotFound)
		return false
	}
	callerIP := r.RemoteAddr
	if h, _, err := net.SplitHostPort(callerIP); err == nil {
		callerIP = h
	}
	var callerApp string
	var ok bool
	if p.authz != nil {
		callerApp, ok = p.authz.AuthorizeCall(callerIP, target)
	}
	if !ok {
		p.log.Debug("ingress: internal call denied", "caller_ip", callerIP, "caller", callerApp, "target", target)
		http.Error(w, "forbidden: app-to-app call not authorized", http.StatusForbidden)
		return false
	}
	return true
}

// wakeAndResolve triggers a coalesced wake of the app for host, then re-resolves
// to read the real post-wake state. It deliberately ignores the wake's own error
// and trusts the re-resolve: a successful wake → a running Target; a failed or
// timed-out wake → the app is still asleep → ErrAsleep (→ 503 upstream); and the
// "someone else already woke it" race resolves straight to a running Target.
// On a successful wake it reports the observed latency via onWake.
func (p *Proxy) wakeAndResolve(ctx context.Context, host string, internal bool) ([]Target, error) {
	resolveSet, appName := p.resolver.ResolveSet, p.resolver.AppName
	if internal {
		resolveSet, appName = p.resolver.ResolveSetInternal, p.resolver.AppNameInternal
	}
	name := appName(host)
	if name == "" {
		return nil, ErrNoRoute
	}
	start := time.Now()
	_ = p.coord.wake(ctx, name)
	set, err := resolveSet(host)
	if err == nil && p.onWake != nil {
		p.onWake(time.Since(start))
	}
	return set, err
}

// Start binds the configured listeners and serves them in the background.
func (p *Proxy) Start() error {
	if p.httpListen != "" {
		ln, err := net.Listen("tcp", p.httpListen)
		if err != nil {
			return err
		}
		// ReadHeaderTimeout guards against Slowloris; IdleTimeout reaps idle
		// keep-alive conns. No Read/WriteTimeout — long proxied bodies
		// (uploads, streaming) are legitimate.
		p.httpSrv = &http.Server{
			Handler:           p,
			ReadHeaderTimeout: httpHeaderTimeout,
			IdleTimeout:       proxyIdleTimeout,
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			if err := p.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				p.log.Error("ingress: http serve", "err", err)
			}
		}()
	}
	if p.internalListen != "" {
		// SO_REUSEPORT: the VIP binds a specific host-local address (the anycast
		// dummy iface), and it must coexist with a published wildcard host port on
		// the same number (e.g. an app running `-p 80:80` while the VIP is on :80).
		// Both sides need SO_REUSEPORT or the second bind hits EADDRINUSE.
		ln, err := reuseport.Listen(p.internalListen)
		if err != nil {
			if p.httpSrv != nil {
				_ = p.httpSrv.Close()
			}
			return err
		}
		// Same L7 routing as the external listener, but over the internal zone
		// (backend.internal). Bound to the anycast VIP, so only guest netns can
		// reach it (enforced by nft), with per-app caller authorization enforced
		// in handle() before any resolve/wake.
		p.internalSrv = &http.Server{
			Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, true) }),
			ReadHeaderTimeout: httpHeaderTimeout,
			IdleTimeout:       proxyIdleTimeout,
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			if err := p.internalSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				p.log.Error("ingress: internal serve", "err", err)
			}
		}()
	}
	if p.tlsListen != "" {
		ln, err := net.Listen("tcp", p.tlsListen)
		if err != nil {
			if p.httpSrv != nil {
				_ = p.httpSrv.Close()
			}
			if p.internalSrv != nil {
				_ = p.internalSrv.Close()
			}
			return err
		}
		p.tlsLn = ln
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.acceptTLS(ln)
		}()
	}
	return nil
}

// Stop shuts the listeners down and waits for the accept loops to exit. In-flight
// SNI splices run to completion on their own (client/guest disconnect).
func (p *Proxy) Stop(ctx context.Context) {
	if p.httpSrv != nil {
		_ = p.httpSrv.Shutdown(ctx)
	}
	if p.internalSrv != nil {
		_ = p.internalSrv.Shutdown(ctx)
	}
	if p.tlsLn != nil {
		_ = p.tlsLn.Close()
	}
	p.wg.Wait()
}

func (p *Proxy) acceptTLS(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		// Bound concurrent handlers: shed (close) connections over the cap so a
		// flood of half-open TLS conns can't exhaust goroutines/FDs.
		select {
		case p.tlsSem <- struct{}{}:
			go func() {
				defer func() { <-p.tlsSem }()
				p.handleSNI(conn)
			}()
		default:
			p.log.Warn("ingress: TLS connection cap reached, shedding", "cap", maxTLSConns)
			_ = conn.Close()
		}
	}
}

// handleSNI peeks the TLS ClientHello for its SNI, resolves the app, and splices
// the raw stream to the current instance — no termination, so the guest owns
// its certificate.
func (p *Proxy) handleSNI(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	sni, hello, err := peekSNI(conn, sniPeekTimeout)
	if err != nil || sni == "" {
		p.log.Debug("ingress: sni peek", "err", err)
		return
	}
	set, err := p.resolver.ResolveSet(sni)
	if errors.Is(err, ErrAsleep) && p.coord != nil {
		// Raw TCP has no request context; bound the wake so a stuck restore
		// can't pin this connection (and its tlsSem slot) indefinitely.
		wctx, cancel := context.WithTimeout(context.Background(), defaultWakeTimeout)
		set, err = p.wakeAndResolve(wctx, sni, false)
		cancel()
	}
	if err != nil {
		p.log.Debug("ingress: sni no route", "sni", sni, "err", err)
		return
	}
	tg, release := p.balancer.Pick(set)
	defer release()
	up, err := net.DialTimeout("tcp", net.JoinHostPort(tg.GuestIP, strconv.Itoa(tg.Port)), dialTimeout)
	if err != nil {
		p.balancer.Fail(tg.InstanceID)
		p.log.Warn("ingress: sni upstream dial", "sni", sni, "err", err)
		return
	}
	defer func() { _ = up.Close() }()
	if _, err := up.Write(hello); err != nil { // replay the buffered ClientHello
		return
	}
	if p.activity != nil {
		name := p.resolver.AppName(sni)
		p.activity.begin(name)
		defer p.activity.end(name)
	}
	pipe(conn, up)
}

// peekConn records everything read from the underlying conn and swallows writes,
// so an aborted TLS handshake can't send an alert back to the client.
type peekConn struct {
	net.Conn
	buf bytes.Buffer
}

func (p *peekConn) Read(b []byte) (int, error) {
	n, err := p.Conn.Read(b)
	if n > 0 {
		p.buf.Write(b[:n])
	}
	return n, err
}

func (p *peekConn) Write(b []byte) (int, error) { return len(b), nil }

// peekSNI reads the TLS ClientHello, extracts its SNI, and returns the raw bytes
// consumed so they can be replayed to the upstream. It aborts the handshake
// right after the ClientHello, so nothing is written back to the client.
func peekSNI(conn net.Conn, timeout time.Duration) (sni string, hello []byte, err error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	pc := &peekConn{Conn: conn}
	stop := errors.New("stop")
	called := false
	hsErr := tls.Server(pc, &tls.Config{
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			called = true
			sni = hi.ServerName
			return nil, stop
		},
	}).HandshakeContext(context.Background())
	if !called {
		return "", pc.buf.Bytes(), hsErr // failed before the ClientHello was parsed
	}
	return sni, pc.buf.Bytes(), nil
}

// pipe copies bidirectionally between two conns with an idle timeout, so a
// spliced connection that goes silent in both directions can't pin goroutines +
// FDs forever. Each write side is half-closed on EOF so a one-way-idle
// connection still drains and terminates.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		copyIdle(dst, src, proxyIdleTimeout)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// copyIdle streams src→dst, resetting src's read deadline before each read so a
// connection idle longer than idle is torn down (the read errors, both copy
// directions unwind, and handleSNI's defers close both conns).
func copyIdle(dst, src net.Conn, idle time.Duration) {
	buf := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(idle))
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}
