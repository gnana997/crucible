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

// maxL4Conns is the GLOBAL ceiling on concurrent app→app L4 connections across every
// per-app VIP, so a flood can't exhaust goroutines/FDs (the L4 analogue of the SNI
// listener's tlsSem). maxPerAppL4Conns is a per-app cap UNDER that ceiling so one app
// (one grantee) cannot consume the whole global budget and starve every other app's L4
// — the fairness bound. Both are enforced for a connection's full lifetime, tcp splice
// and http keep-alive alike (the slot releases on the connection's Close).
const (
	maxL4Conns       = 4096
	maxPerAppL4Conns = 256

	// l4SourceQPS/Burst rate-limit new connections per SOURCE guest IP, checked
	// BEFORE authorization so a guest cannot churn connections to drive unbounded
	// per-connection authorization work (the caller→app store lookup). Mirrors the
	// DNS proxy's per-source limiter. A legit pooled client stays well under this.
	l4SourceQPS   = 50
	l4SourceBurst = 100
)

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
	sem chan struct{} // global ceiling (maxL4Conns)

	mu      sync.Mutex
	apps    map[string][]*l4Listener
	stopped bool

	inflMu sync.Mutex     // guards infl (hot per-conn path; kept off mu/app-lifecycle)
	infl   map[string]int // per-app in-flight connection count (cap maxPerAppL4Conns)

	srcMu   sync.Mutex              // guards srcRate
	srcRate map[string]*tokenBucket // per-source-guest-IP connection rate limiter

	httpOnce  sync.Once
	httpSrv   *http.Server
	httpConns *connChanListener

	wg sync.WaitGroup
}

func newL4Manager(p *Proxy) *l4Manager {
	return &l4Manager{
		p:       p,
		sem:     make(chan struct{}, maxL4Conns),
		apps:    map[string][]*l4Listener{},
		infl:    map[string]int{},
		srcRate: map[string]*tokenBucket{},
	}
}

// tokenBucket is a minimal rate limiter (the small subset we need; golang.org/x/time
// is not a dependency). Mirrors internal/network/dnsproxy's rateLimiter.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
	}
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allowSource returns false when the source guest IP is over its connection rate
// limit. The srcRate map is bounded by the guest-IP space (a guest cannot spoof its
// source — nft binds it to the veth), so it needs no eviction.
func (m *l4Manager) allowSource(ip string) bool {
	m.srcMu.Lock()
	tb := m.srcRate[ip]
	if tb == nil {
		tb = &tokenBucket{rate: l4SourceQPS, burst: l4SourceBurst, tokens: l4SourceBurst}
		m.srcRate[ip] = tb
	}
	m.srcMu.Unlock()
	return tb.allow(time.Now())
}

// hostOnly strips the port from a "host:port" address.
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// acquire reserves a connection slot for app under BOTH the global ceiling and the
// per-app cap, returning false (nothing reserved) if either is full. release returns
// the slot. Together they stop one app from monopolizing the global budget.
func (m *l4Manager) acquire(app string) bool {
	select {
	case m.sem <- struct{}{}: // global ceiling
	default:
		return false
	}
	m.inflMu.Lock()
	if m.infl[app] >= maxPerAppL4Conns {
		m.inflMu.Unlock()
		<-m.sem // give the global slot back
		return false
	}
	m.infl[app]++
	m.inflMu.Unlock()
	return true
}

// onConn reports an L4 connection outcome to the metric hook (if wired).
func (m *l4Manager) onConn(outcome string) {
	if m.p.onL4Conn != nil {
		m.p.onL4Conn(outcome)
	}
}

func (m *l4Manager) release(app string) {
	m.inflMu.Lock()
	if m.infl[app] > 0 {
		m.infl[app]--
		if m.infl[app] == 0 {
			delete(m.infl, app)
		}
	}
	m.inflMu.Unlock()
	<-m.sem
}

// slotConn releases a connection slot (global + per-app) exactly once, when the
// connection is closed. This bounds a connection for its FULL lifetime — an http
// keep-alive conn living in the shared L4 http.Server holds its slot until it closes,
// not just until the accept goroutine hands it off (the fix for the http-cap gap).
type slotConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *slotConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// l4Listener is one bound VIP:port and its accept loop.
type l4Listener struct {
	ln     net.Listener
	app    string
	proto  string
	port   int // the declared internal port — also the guest port to dial (tcp path)
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
		l := &l4Listener{ln: ln, app: app, proto: proto, port: pt.Port, m: m}
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
		// Per-source rate limit FIRST, before any authorization work, so a guest
		// churning connections can't drive unbounded caller→app lookups.
		if !l.m.allowSource(hostOnly(c.RemoteAddr().String())) {
			l.m.p.log.Debug("ingress: L4 source rate-limited", "app", l.app, "src", c.RemoteAddr())
			l.m.onConn("shed_rate")
			_ = c.Close()
			continue
		}
		// Reserve a slot under the global ceiling AND the per-app cap; shed over
		// either rather than spawning unboundedly or letting one app starve others.
		// The slot is held until the connection CLOSES (via slotConn), so an http
		// keep-alive conn counts for its whole lifetime too, not just the hand-off.
		if !l.m.acquire(l.app) {
			l.m.p.log.Warn("ingress: L4 connection cap reached, shedding",
				"app", l.app, "per_app_cap", maxPerAppL4Conns, "global_cap", maxL4Conns)
			l.m.onConn("shed_cap")
			_ = c.Close()
			continue
		}
		app := l.app
		sc := &slotConn{Conn: c, release: func() { l.m.release(app) }}
		l.m.wg.Add(1)
		go func() {
			defer l.m.wg.Done()
			if l.proto == "http" {
				if !l.m.httpConns.push(sc) {
					_ = sc.Close() // releases the slot
				}
				return
			}
			l.m.handleTCP(l.app, l.port, sc) // its deferred Close releases the slot
		}()
	}
}

// handleTCP is the blind L4 splice: authorize the caller (default-deny, BEFORE any
// wake), resolve the app's endpoint set (waking a slept callee), load-balance, dial,
// and splice. Mirrors handleSNI minus the SNI peek — the app is known from the listener.
func (m *l4Manager) handleTCP(app string, guestPort int, client net.Conn) {
	defer func() { _ = client.Close() }()
	p := m.p
	l4conn := m.onConn

	callerIP := hostOnly(client.RemoteAddr().String())
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
	set, err := p.resolveSetWakingPort(ctx, app, guestPort)
	cancel()
	if err != nil {
		p.log.Debug("ingress: L4 no route", "app", app, "port", guestPort, "err", err)
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

// resolveSetWakingPort resolves an app's endpoint set on an explicit guest port (the
// L4 listener's declared port), waking a slept app (coalesced across a herd) and
// re-resolving. Mirrors wakeAndResolve but keyed by app name + port (the L4 listener
// already knows which app it fronts and on which port).
func (p *Proxy) resolveSetWakingPort(ctx context.Context, name string, port int) ([]Target, error) {
	set, err := p.resolver.resolveSetPort(name, port)
	if errors.Is(err, ErrAsleep) && p.coord != nil {
		start := time.Now()
		_ = p.coord.wake(ctx, name)
		set, err = p.resolver.resolveSetPort(name, port)
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
