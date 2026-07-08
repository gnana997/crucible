// Package portpublish implements host port publishing for sandboxes: a
// per-mapping TCP forwarder that accepts on a host address and pipes
// each connection to a guest IP:port. It is the daemon-side, userland
// equivalent of `docker run -p` (Docker's userland proxy), deliberately
// chosen over kernel DNAT so localhost publishing works without the
// route_localnet sysctl and so there is no ingress nftables state to
// reap — a forwarder is an in-memory goroutine that dies with the
// daemon.
//
// It is a dumb per-port pipe to one guest, not a request router: no
// host-header/SNI/TLS. The routing proxy is a later item.
package portpublish

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// dialTimeout bounds each backend dial so a wedged guest can't pile up
// blocked accept goroutines.
const dialTimeout = 5 * time.Second

// Mapping is one host→guest forward.
type Mapping struct {
	HostIP    string // bind address; "" means 0.0.0.0
	HostPort  int
	GuestIP   string // the sandbox guest IP to dial
	GuestPort int
}

func (m Mapping) hostAddr() string {
	return net.JoinHostPort(m.HostIP, strconv.Itoa(m.HostPort)) // HostIP "" → ":port" (all interfaces)
}

func (m Mapping) guestAddr() string {
	return net.JoinHostPort(m.GuestIP, strconv.Itoa(m.GuestPort))
}

// forwarder owns one listener and the connections it accepts.
type forwarder struct {
	ln       net.Listener
	guest    string // guest dial address
	log      *slog.Logger
	wg       sync.WaitGroup
	closing  chan struct{}
	closeOne sync.Once
}

// Set is a sandbox's group of forwarders, closed together on teardown.
type Set struct {
	fwds []*forwarder
}

// Publish binds a listener per mapping and starts forwarding. On any
// bind failure it closes everything already started and returns the
// error, so a partially-published sandbox never lingers (the caller
// rolls the create back).
func Publish(log *slog.Logger, mappings []Mapping) (*Set, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Set{}
	for _, m := range mappings {
		ln, err := net.Listen("tcp", m.hostAddr())
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("portpublish: bind %s: %w", m.hostAddr(), err)
		}
		f := &forwarder{
			ln:      ln,
			guest:   m.guestAddr(),
			log:     log.With("component", "portpublish", "host", m.hostAddr(), "guest", m.guestAddr()),
			closing: make(chan struct{}),
		}
		s.fwds = append(s.fwds, f)
		go f.accept()
	}
	return s, nil
}

// Close stops every forwarder: close the listeners (unblocking Accept),
// then wait for in-flight connections to drain.
func (s *Set) Close() {
	if s == nil {
		return
	}
	for _, f := range s.fwds {
		f.stop()
	}
	for _, f := range s.fwds {
		f.wg.Wait()
	}
}

func (f *forwarder) stop() {
	f.closeOne.Do(func() {
		close(f.closing)
		_ = f.ln.Close()
	})
}

func (f *forwarder) accept() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			select {
			case <-f.closing:
				return // listener closed by stop() — expected
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

// handle dials the guest and pipes bytes both ways until either side
// closes. Half-close is propagated (CloseWrite) so a client that shuts
// down its write half — HTTP keep-alive, streaming uploads — doesn't
// prematurely tear down the response direction.
func (f *forwarder) handle(client net.Conn) {
	defer func() { _ = client.Close() }()

	backend, err := net.DialTimeout("tcp", f.guest, dialTimeout)
	if err != nil {
		// The service inside the guest may not have bound yet, or the
		// guest port isn't listening — same as connecting to a stopped
		// container: the client sees the connection close.
		f.log.Debug("dial guest failed", "err", err)
		return
	}
	defer func() { _ = backend.Close() }()

	// Also abort both copies promptly if the sandbox is torn down.
	go func() {
		<-f.closing
		_ = client.Close()
		_ = backend.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, backend, client) // client → guest
	go pipe(&wg, client, backend) // guest → client
	wg.Wait()
}

// pipe copies src→dst and half-closes dst's write side at EOF so the
// peer observes the shutdown.
func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = dst.Close()
	}
}
