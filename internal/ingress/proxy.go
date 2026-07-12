package ingress

import (
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
)

const (
	dialTimeout       = 5 * time.Second
	sniPeekTimeout    = 10 * time.Second
	httpHeaderTimeout = 10 * time.Second // Slowloris guard on the HTTP listener
	proxyIdleTimeout  = 5 * time.Minute  // close a spliced/keep-alive conn idle this long
	maxTLSConns       = 1024             // cap concurrent SNI-passthrough conns
)

type targetKey struct{}

// Config configures the ingress proxy.
type Config struct {
	Resolver   *Resolver
	HTTPListen string // e.g. ":80"; empty disables the HTTP (host-header) proxy
	TLSListen  string // e.g. ":443"; empty disables the TLS (SNI-passthrough) proxy
	Logger     *slog.Logger
}

// Proxy is the daemon-owned ingress front door: :80 host-header routing (L7,
// via httputil.ReverseProxy) and :443 SNI passthrough (L4, no TLS termination —
// the guest owns its cert), both routed to an app's current instance via the
// Resolver. In-process, mirroring the DNS proxy.
type Proxy struct {
	resolver   *Resolver
	log        *slog.Logger
	httpListen string
	tlsListen  string

	rp      *httputil.ReverseProxy
	httpSrv *http.Server
	tlsLn   net.Listener
	tlsSem  chan struct{} // bounds concurrent SNI-passthrough handlers
	wg      sync.WaitGroup
}

// New builds a proxy from cfg. Call Start to bind and serve.
func New(cfg Config) *Proxy {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	p := &Proxy{
		resolver:   cfg.Resolver,
		log:        log,
		httpListen: cfg.HTTPListen,
		tlsListen:  cfg.TLSListen,
		tlsSem:     make(chan struct{}, maxTLSConns),
	}
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
			p.log.Warn("ingress: upstream error", "host", r.Host, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p
}

// ServeHTTP resolves the app from the request Host and reverse-proxies to its
// current instance. Unknown host → 404; app with no ready instance → 502.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tg, err := p.resolver.Resolve(r.Host)
	if err != nil {
		switch {
		case errors.Is(err, ErrAsleep):
			// M2-4 replaces this with wake-and-forward; until then a slept app
			// is a clean 503 (not a misleading 404).
			http.Error(w, "app is asleep", http.StatusServiceUnavailable)
		case errors.Is(err, ErrNoInstance):
			http.Error(w, "app has no ready instance", http.StatusBadGateway)
		default:
			http.Error(w, "no such app", http.StatusNotFound)
		}
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), targetKey{}, tg))
	p.rp.ServeHTTP(w, r)
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
	if p.tlsListen != "" {
		ln, err := net.Listen("tcp", p.tlsListen)
		if err != nil {
			if p.httpSrv != nil {
				_ = p.httpSrv.Close()
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
	tg, err := p.resolver.Resolve(sni)
	if err != nil {
		p.log.Debug("ingress: sni no route", "sni", sni, "err", err)
		return
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort(tg.GuestIP, strconv.Itoa(tg.Port)), dialTimeout)
	if err != nil {
		p.log.Warn("ingress: sni upstream dial", "sni", sni, "err", err)
		return
	}
	defer func() { _ = up.Close() }()
	if _, err := up.Write(hello); err != nil { // replay the buffered ClientHello
		return
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
