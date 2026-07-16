package ingress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gnana997/crucible/internal/reuseport"
)

// maxL4Conns bounds concurrent app→app L4 connections across all per-app VIPs, so a
// flood of half-open conns can't exhaust goroutines/FDs (the L4 analogue of the SNI
// listener's tlsSem).
const maxL4Conns = 4096

// L4Port is one internal TCP port an app exposes app→app (v0.9.5), with how the proxy
// handles it: "tcp" (default) = blind byte splice — any protocol, TLS passes through
// untouched (what a database endpoint needs); "http" = hand the connection to the L7
// server for per-request routing, load-balancing, and status metrics.
type L4Port struct {
	Port  int
	Proto string // "tcp" | "http"
}

// l4Manager binds per-app VIP:port listeners and dispatches accepted connections by
// protocol. It is owned by the Proxy; AddInternalApp/RemoveInternalApp drive it from
// app lifecycle. A single shared http.Server (fed via a connChanListener) serves every
// "http" port — it routes by the guest's Host: <app>.internal, so one server suffices.
type l4Manager struct {
	p   *Proxy
	sem chan struct{}

	mu      sync.Mutex
	apps    map[string][]*l4Listener
	stopped bool

	httpOnce  sync.Once
	httpSrv   *http.Server
	httpConns *connChanListener

	wg sync.WaitGroup
}

func newL4Manager(p *Proxy) *l4Manager {
	return &l4Manager{
		p:    p,
		sem:  make(chan struct{}, maxL4Conns),
		apps: map[string][]*l4Listener{},
	}
}

// l4Listener is one bound VIP:port and its accept loop.
type l4Listener struct {
	ln     net.Listener
	app    string
	proto  string
	m      *l4Manager
	closed atomic.Bool
}

func (l *l4Listener) close() {
	if l.closed.Swap(true) {
		return
	}
	_ = l.ln.Close()
}

// AddInternalApp binds the app's per-app VIP for each declared port and starts serving
// app→app traffic to it. Idempotent per app: a second call for an app that is already
// bound is a no-op (RemoveInternalApp first to rebind after a port change). Partial
// bind failures roll back the ports already bound for this app.
func (p *Proxy) AddInternalApp(app string, vip netip.Addr, ports []L4Port) error {
	if p.l4 == nil {
		return errors.New("ingress: L4 app→app not enabled")
	}
	return p.l4.addApp(app, vip, ports)
}

// RemoveInternalApp stops serving an app's per-app VIP and closes its listeners.
func (p *Proxy) RemoveInternalApp(app string) {
	if p.l4 != nil {
		p.l4.removeApp(app)
	}
}

func (m *l4Manager) addApp(app string, vip netip.Addr, ports []L4Port) error {
	if !vip.IsValid() {
		return fmt.Errorf("ingress: app %q has no valid VIP", app)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return errors.New("ingress: proxy stopped")
	}
	if _, ok := m.apps[app]; ok {
		return nil // already bound
	}

	var bound []*l4Listener
	for _, pt := range ports {
		proto := pt.Proto
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "http" {
			for _, b := range bound {
				b.close()
			}
			return fmt.Errorf("ingress: app %q port %d: unknown proto %q", app, pt.Port, proto)
		}
		addr := net.JoinHostPort(vip.String(), strconv.Itoa(pt.Port))
		// SO_REUSEPORT: a per-app VIP:port must coexist with a wildcard published host
		// port on the same number (same reason the anycast internal listener uses it).
		ln, err := reuseport.Listen(addr)
		if err != nil {
			for _, b := range bound {
				b.close()
			}
			return fmt.Errorf("ingress: bind L4 %s for %q: %w", addr, app, err)
		}
		if proto == "http" {
			m.ensureHTTP()
		}
		l := &l4Listener{ln: ln, app: app, proto: proto, m: m}
		bound = append(bound, l)
		m.wg.Add(1)
		go l.accept()
	}
	m.apps[app] = bound
	m.p.log.Debug("ingress: L4 app bound", "app", app, "vip", vip, "ports", len(ports))
	return nil
}

func (m *l4Manager) removeApp(app string) {
	m.mu.Lock()
	ls := m.apps[app]
	delete(m.apps, app)
	m.mu.Unlock()
	for _, l := range ls {
		l.close()
	}
}

// ensureHTTP lazily starts the shared L7 server that serves every "http" L4 port. Its
// handler is the internal-zone L7 path (authz + resolve + wake + per-request LB), the
// same one the anycast internal listener uses.
func (m *l4Manager) ensureHTTP() {
	m.httpOnce.Do(func() {
		m.httpConns = newConnChanListener(&net.TCPAddr{})
		m.httpSrv = &http.Server{
			Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.p.handle(w, r, true) }),
			ReadHeaderTimeout: httpHeaderTimeout,
			IdleTimeout:       proxyIdleTimeout,
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			if err := m.httpSrv.Serve(m.httpConns); err != nil &&
				!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				m.p.log.Error("ingress: L4 http serve", "err", err)
			}
		}()
	})
}

func (l *l4Listener) accept() {
	defer l.m.wg.Done()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if l.closed.Load() {
				return
			}
			time.Sleep(10 * time.Millisecond) // transient accept error; keep serving
			continue
		}
		// Shed over the concurrency cap rather than spawning unboundedly.
		select {
		case l.m.sem <- struct{}{}:
		default:
			l.m.p.log.Warn("ingress: L4 connection cap reached, shedding", "cap", maxL4Conns, "app", l.app)
			_ = c.Close()
			continue
		}
		l.m.wg.Add(1)
		go func() {
			defer l.m.wg.Done()
			defer func() { <-l.m.sem }()
			if l.proto == "http" {
				if !l.m.httpConns.push(c) {
					_ = c.Close()
				}
				return
			}
			l.m.handleTCP(l.app, c)
		}()
	}
}

// handleTCP is the blind L4 splice: authorize the caller (default-deny, BEFORE any
// wake), resolve the app's endpoint set (waking a slept callee), load-balance, dial,
// and splice. Mirrors handleSNI minus the SNI peek — the app is known from the listener.
func (m *l4Manager) handleTCP(app string, client net.Conn) {
	defer func() { _ = client.Close() }()
	p := m.p
	l4conn := func(outcome string) {
		if p.onL4Conn != nil {
			p.onL4Conn(outcome)
		}
	}

	callerIP := client.RemoteAddr().String()
	if h, _, err := net.SplitHostPort(callerIP); err == nil {
		callerIP = h
	}
	// Fail closed: no authorizer, or an unauthorized/unknown caller, never even wakes
	// the callee.
	if p.authz == nil {
		l4conn("denied")
		return
	}
	callerApp, ok := p.authz.AuthorizeCall(callerIP, app)
	if !ok {
		p.log.Debug("ingress: L4 internal call denied", "caller_ip", callerIP, "caller", callerApp, "target", app)
		l4conn("denied")
		return
	}
	if p.onInternal != nil {
		p.onInternal()
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultWakeTimeout)
	set, err := p.resolveSetWaking(ctx, app)
	cancel()
	if err != nil {
		p.log.Debug("ingress: L4 no route", "app", app, "err", err)
		l4conn("no_route")
		return
	}
	tg, release := p.balancer.Pick(set)
	defer release()
	up, err := net.DialTimeout("tcp", net.JoinHostPort(tg.GuestIP, strconv.Itoa(tg.Port)), dialTimeout)
	if err != nil {
		p.balancer.Fail(tg.InstanceID)
		p.log.Warn("ingress: L4 upstream dial", "app", app, "err", err)
		l4conn("dial_error")
		return
	}
	defer func() { _ = up.Close() }()
	if p.activity != nil {
		p.activity.begin(app)
		defer p.activity.end(app)
	}
	l4conn("spliced")
	// Count total bytes both directions: reads from the client are client→guest,
	// writes to the client are guest→client, so tallying the client side captures
	// both. Reported once on close (not per packet).
	cc := &countingConn{Conn: client}
	if p.onL4Bytes != nil {
		defer func() { p.onL4Bytes(cc.n.Load()) }()
	}
	pipe(cc, up)
}

// countingConn tallies bytes read + written on a wrapped conn (an atomic so the two
// pipe copy goroutines can update it concurrently). Reported once at close.
type countingConn struct {
	net.Conn
	n atomic.Int64
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.n.Add(int64(n))
	return n, err
}

// resolveSetWaking resolves an app's endpoint set by name, waking a slept app
// (coalesced across a herd) and re-resolving. Mirrors wakeAndResolve but keyed by app
// name (the L4 listener already knows which app it fronts).
func (p *Proxy) resolveSetWaking(ctx context.Context, name string) ([]Target, error) {
	set, err := p.resolver.resolveSet(name)
	if errors.Is(err, ErrAsleep) && p.coord != nil {
		start := time.Now()
		_ = p.coord.wake(ctx, name)
		set, err = p.resolver.resolveSet(name)
		if err == nil && p.onWake != nil {
			p.onWake(time.Since(start))
		}
	}
	return set, err
}

// stop closes every per-app listener and the shared L4 http server, then waits for
// accept loops to exit. In-flight splices tear down on their own conn close.
func (m *l4Manager) stop(ctx context.Context) {
	m.mu.Lock()
	m.stopped = true
	var all []*l4Listener
	for _, ls := range m.apps {
		all = append(all, ls...)
	}
	m.apps = map[string][]*l4Listener{}
	m.mu.Unlock()

	for _, l := range all {
		l.close()
	}
	if m.httpSrv != nil {
		_ = m.httpSrv.Shutdown(ctx)
	}
	if m.httpConns != nil {
		_ = m.httpConns.Close()
	}
	m.wg.Wait()
}
