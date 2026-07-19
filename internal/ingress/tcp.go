package ingress

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gnana997/crucible/internal/reuseport"
)

// keepAlivePeriod is the TCP keepalive probe interval set on connections in
// keep-connections mode (reaping off): a genuinely dead peer is closed within a
// few probes so it can't pin the app awake forever, while a live-but-idle
// connection's TCP stack ACKs the probes and stays up.
const keepAlivePeriod = 60 * time.Second

// guestReadyTimeout bounds how long the waking forwarder HOLDS a client while it
// retries the dial to a just-woken guest whose service is still ramping. "App
// running" (health = TCP-accept) does not guarantee the service will accept a
// BURST the instant it wakes: under lazy-paging its accept() stalls and a
// concurrent burst overflows the guest's listen backlog, so a single dial can
// fail. The client's connect is held — never reset — until the guest accepts or
// this budget elapses (only then does a genuinely-down guest close the client).
const guestReadyTimeout = 20 * time.Second

// WakingForwarder is the L4 analogue of the ingress proxy: a raw TCP listener
// that fronts one app's published port and, on each connection, resolves the
// app's current instance, WAKES it if asleep (holding the connection until it is
// running), then pipes bytes to the live guest. Unlike internal/portpublish —
// a dumb pipe to a fixed guest IP that dies with the VM — this is app-scoped and
// resolves fresh per connection, so it survives sleep and re-targets a new guest
// IP after a stop/start wake (a volume app cold-creates a fresh instance).
//
// It records connection activity into the shared ActivityTracker so the idle
// monitor can auto-sleep the app when it has been idle (zero open connections)
// for its idle_timeout.
type WakingForwarder struct {
	appName   string
	guestPort int // the guest port to dial (from the publish mapping)

	ln        net.Listener
	resolve   func(name string) (Target, error) // Resolver.ResolveName
	coord     *wakeCoordinator                  // coalesces a herd of connects into one wake
	activity  *ActivityTracker                  // shared with the idle monitor; may be nil
	reapIdle  time.Duration                     // close a connection idle this long; 0 = never (keep-connections)
	keepAlive bool                              // set SO_KEEPALIVE (reaps a dead peer when reapIdle==0)
	log       *slog.Logger

	wg       sync.WaitGroup
	closing  chan struct{}
	closeOne sync.Once
}

// WakingForwarderConfig configures a WakingForwarder. ReapIdle is how long a
// connection may sit byte-idle before the forwarder closes it (so pooled clients
// let a scale-to-zero app reach zero connections); 0 disables reaping
// (keep-connections mode), in which case set KeepAlive so a dead peer is still
// reaped by TCP keepalive.
type WakingForwarderConfig struct {
	HostAddr  string
	AppName   string
	GuestPort int
	Resolver  *Resolver
	Waker     Waker
	Activity  *ActivityTracker
	ReapIdle  time.Duration
	KeepAlive bool
	Log       *slog.Logger
}

// NewWakingForwarder binds HostAddr (SO_REUSEPORT, so a published wildcard port
// still coexists with the app→app VIP on the same port) and starts accepting.
// It seeds activity for AppName so a never-connected scale-to-zero app still
// becomes eligible to sleep after its idle_timeout.
func NewWakingForwarder(cfg WakingForwarderConfig) (*WakingForwarder, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	ln, err := reuseport.Listen(cfg.HostAddr)
	if err != nil {
		return nil, err
	}
	f := &WakingForwarder{
		appName:   cfg.AppName,
		guestPort: cfg.GuestPort,
		ln:        ln,
		resolve:   cfg.Resolver.ResolveName,
		coord:     newWakeCoordinator(cfg.Waker, 0),
		activity:  cfg.Activity,
		reapIdle:  cfg.ReapIdle,
		keepAlive: cfg.KeepAlive,
		log:       log.With("component", "l4wake", "app", cfg.AppName, "host", cfg.HostAddr, "guest_port", cfg.GuestPort),
		closing:   make(chan struct{}),
	}
	if cfg.Activity != nil {
		cfg.Activity.Seen(cfg.AppName)
	}
	f.wg.Add(1)
	go f.acceptLoop()
	return f, nil
}

// Addr is the bound host address (useful in tests with a :0 port).
func (f *WakingForwarder) Addr() net.Addr { return f.ln.Addr() }

// Close stops accepting, unblocks any in-flight wake wait, and drains
// connections before returning.
func (f *WakingForwarder) Close() {
	if f == nil {
		return
	}
	f.closeOne.Do(func() {
		close(f.closing)
		_ = f.ln.Close()
	})
	f.wg.Wait()
}

func (f *WakingForwarder) acceptLoop() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			select {
			case <-f.closing:
				return // listener closed by Close() — expected
			default:
				f.log.Warn("accept failed", "err", err)
				return
			}
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handle(conn)
		}()
	}
}

// handle resolves (waking if needed) the current instance, dials it, and pipes.
// activity.begin/end bracket the whole connection so the idle monitor counts it
// as in-flight for its lifetime and starts the idle clock from its close.
func (f *WakingForwarder) handle(client net.Conn) {
	defer func() { _ = client.Close() }()

	if f.activity != nil {
		f.activity.begin(f.appName)
		defer f.activity.end(f.appName)
	}

	target, err := f.target()
	if err != nil {
		// Asleep-and-wake-failed, no ready instance, or unknown app: close the
		// client, same as connecting to a stopped container.
		f.log.Debug("no target for connection", "err", err)
		return
	}

	// Hold the client through the guest's post-wake ramp: retry the dial with short
	// backoff so a BURST landing on a just-woken app is held until the service
	// accepts, never reset. No client bytes are read before this (the startup
	// packet stays buffered in the socket), so nothing is lost by waiting. Only
	// after guestReadyTimeout do we give up (genuinely-down guest → close).
	guestAddr := net.JoinHostPort(target.GuestIP, strconv.Itoa(f.guestPort))
	var backend net.Conn
	deadline := time.Now().Add(guestReadyTimeout)
	for {
		backend, err = net.DialTimeout("tcp", guestAddr, dialTimeout)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			f.log.Debug("dial guest failed (ready budget elapsed)", "guest_ip", target.GuestIP, "err", err)
			return
		}
		select {
		case <-f.closing:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { _ = backend.Close() }()

	// Keep-connections mode (reapIdle==0): don't reap on silence, but enable TCP
	// keepalive on both ends so a genuinely dead peer can't pin the app awake.
	if f.keepAlive {
		setKeepAlive(client)
		setKeepAlive(backend)
	}

	// Abort both ends promptly if the forwarder is torn down (app deleted); the
	// watcher exits when the pipe completes, so it never outlives the connection.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-f.closing:
			_ = client.Close()
			_ = backend.Close()
		case <-done:
		}
	}()

	// Bidirectional splice with half-close. reapIdle bounds byte-idleness: a
	// connection silent that long is closed so a scale-to-zero app can reach zero
	// connections and sleep (0 = never reap — keep-connections mode).
	pipeWithIdle(client, backend, f.reapIdle)
}

// setKeepAlive enables TCP keepalive with keepAlivePeriod on a connection, if it
// is a *net.TCPConn. Best-effort.
func setKeepAlive(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(keepAlivePeriod)
	}
}

// target resolves the app's current instance, waking it (coalesced) on ErrAsleep
// and re-resolving to read the real post-wake state — mirroring the HTTP proxy's
// wakeAndResolve: it trusts the re-resolve, not the wake's own error (a still-
// asleep app resolves to ErrAsleep → close; a raced wake resolves to a Target).
// The wake wait is cancelled if the forwarder closes, so teardown never blocks on
// a pending wake. No client bytes are read before this returns, so nothing sent
// on connect (e.g. a postgres startup packet) is lost — it waits in the socket.
func (f *WakingForwarder) target() (Target, error) {
	t, err := f.resolve(f.appName)
	if !errors.Is(err, ErrAsleep) {
		return t, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	_ = f.coord.wake(ctx, f.appName)
	return f.resolve(f.appName)
}
