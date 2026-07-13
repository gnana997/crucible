package ingress

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/gnana997/crucible/internal/reuseport"
)

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

	ln       net.Listener
	resolve  func(name string) (Target, error) // Resolver.ResolveName
	coord    *wakeCoordinator                  // coalesces a herd of connects into one wake
	activity *ActivityTracker                  // shared with the idle monitor; may be nil
	log      *slog.Logger

	wg       sync.WaitGroup
	closing  chan struct{}
	closeOne sync.Once
}

// NewWakingForwarder binds hostAddr (SO_REUSEPORT, so a published wildcard port
// still coexists with the app→app VIP on the same port) and starts accepting.
// It seeds activity for appName so a never-connected scale-to-zero app still
// becomes eligible to sleep after its idle_timeout.
func NewWakingForwarder(hostAddr, appName string, guestPort int, r *Resolver, waker Waker, act *ActivityTracker, log *slog.Logger) (*WakingForwarder, error) {
	if log == nil {
		log = slog.Default()
	}
	ln, err := reuseport.Listen(hostAddr)
	if err != nil {
		return nil, err
	}
	f := &WakingForwarder{
		appName:   appName,
		guestPort: guestPort,
		ln:        ln,
		resolve:   r.ResolveName,
		coord:     newWakeCoordinator(waker, 0),
		activity:  act,
		log:       log.With("component", "l4wake", "app", appName, "host", hostAddr, "guest_port", guestPort),
		closing:   make(chan struct{}),
	}
	if act != nil {
		act.Seen(appName)
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

	backend, err := net.DialTimeout("tcp", net.JoinHostPort(target.GuestIP, strconv.Itoa(f.guestPort)), dialTimeout)
	if err != nil {
		f.log.Debug("dial guest failed", "guest_ip", target.GuestIP, "err", err)
		return
	}
	defer func() { _ = backend.Close() }()

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

	// Bidirectional splice with the proxy's half-close + both-idle safety net,
	// the same helper the L7 SNI passthrough uses for its raw-TCP forward.
	pipe(client, backend)
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
