//go:build linux

package dhcp

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// netnsRoot matches internal/network/netns.go's constant. Duplicated
// here rather than imported to keep this subpackage dependency-free
// on its parent.
const netnsRoot = "/var/run/netns"

// Config is the minimal set of fields a per-netns responder needs.
// The caller constructs one per sandbox; the responder owns a
// single lease and answers for exactly one client MAC.
type Config struct {
	// Netns is the name (without /var/run/netns prefix) the
	// responder binds UDP/67 inside.
	Netns string

	// BindDevice pins the socket to a specific L3 interface via
	// SO_BINDTODEVICE. Required: our sandbox netns has no default
	// route, so WriteTo(255.255.255.255:68) would otherwise fail
	// with ENETUNREACH. Binding to the bridge (or tap) lets the
	// kernel egress broadcasts without a route lookup. Callers
	// pass the in-netns bridge name.
	BindDevice string

	// ClientMAC is the guest's expected MAC, used in logs for
	// "which sandbox is this?" correlation. We no longer drop
	// packets whose source MAC differs (forks restore from
	// snapshot and inherit the source's MAC; dropping would make
	// their DHCP fail). Netns isolation + SO_BINDTODEVICE are
	// sufficient containment.
	ClientMAC [6]byte

	// OfferedIP is the single IP we hand out. Every OFFER/ACK
	// contains this value in yiaddr.
	OfferedIP netip.Addr

	// Gateway, DNS, and SubnetMask populate the OFFER/ACK options.
	// SubnetMask is typically derived via MaskFromPrefixBits from
	// the sandbox's /30 lease.
	Gateway    netip.Addr
	DNS        netip.Addr
	SubnetMask [4]byte

	// LeaseSeconds is option 51's value. Short (60s) by default so
	// a forked guest whose eth0 has stale state from the source
	// triggers a renewal within one half-lease; the agent-side
	// RefreshNetwork endpoint makes this near-immediate, but the
	// short lease is the belt-and-suspenders fallback.
	LeaseSeconds uint32

	// Logger receives server lifecycle events and transaction
	// summaries at info level; bad-packet diagnostics at debug.
	Logger *slog.Logger
}

// validate checks invariants before the responder starts.
func (c Config) validate() error {
	if c.Netns == "" {
		return errors.New("dhcp: Netns required")
	}
	if c.BindDevice == "" {
		return errors.New("dhcp: BindDevice required")
	}
	if !c.OfferedIP.Is4() {
		return errors.New("dhcp: OfferedIP must be IPv4")
	}
	if !c.Gateway.Is4() {
		return errors.New("dhcp: Gateway must be IPv4")
	}
	if !c.DNS.Is4() {
		return errors.New("dhcp: DNS must be IPv4")
	}
	if c.LeaseSeconds == 0 {
		return errors.New("dhcp: LeaseSeconds must be > 0")
	}
	return nil
}

// Responder is a running per-netns DHCP server. Construct with
// Start; stop with Stop. Safe to Stop multiple times.
type Responder struct {
	cfg     Config
	conn    net.PacketConn
	stopped chan struct{}
	stopOnce sync.Once
	log     *slog.Logger
}

// Start launches a responder goroutine bound inside cfg.Netns.
// Returns once the socket is successfully bound (so callers know
// DHCP is live before they resume the guest VM). On any pre-bind
// failure, returns an error and no goroutine is running.
func Start(cfg Config) (*Responder, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "dhcp", "netns", cfg.Netns)

	nsPath := filepath.Join(netnsRoot, cfg.Netns)
	nsFd, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("dhcp: open netns %s: %w", nsPath, err)
	}

	// Use a bound channel to hand the opened listener back from
	// the netns-tainted goroutine. A single-element buffer means
	// the sender never blocks; we either get the conn or an error.
	type bound struct {
		conn net.PacketConn
		err  error
	}
	ch := make(chan bound, 1)

	r := &Responder{cfg: cfg, stopped: make(chan struct{}), log: log}

	go func() {
		// Taint this OS thread by entering the target netns and
		// never returning — we let the thread die along with this
		// goroutine when Stop closes the listener. The alternative
		// (Setns back to root) would require saving a second fd
		// and is error-prone in the error paths.
		runtime.LockOSThread()

		if err := unix.Setns(nsFd, unix.CLONE_NEWNET); err != nil {
			unix.Close(nsFd)
			ch <- bound{err: fmt.Errorf("dhcp: setns: %w", err)}
			return
		}
		unix.Close(nsFd)

		// Bind UDP/67 inside the now-current netns.
		conn, err := net.ListenPacket("udp4", ":67")
		if err != nil {
			ch <- bound{err: fmt.Errorf("dhcp: bind :67: %w", err)}
			return
		}

		// Enable SO_BROADCAST so we can reply to 255.255.255.255
		// (the client has no IP yet).
		if err := enableBroadcast(conn); err != nil {
			_ = conn.Close()
			ch <- bound{err: fmt.Errorf("dhcp: SO_BROADCAST: %w", err)}
			return
		}

		// Pin the socket to the in-netns bridge (or tap) interface.
		// The netns has no default route, so the kernel's route
		// lookup for 255.255.255.255 would fail with ENETUNREACH.
		// SO_BINDTODEVICE sidesteps the route lookup and sends the
		// frame directly out the named device.
		if err := bindToDevice(conn, cfg.BindDevice); err != nil {
			_ = conn.Close()
			ch <- bound{err: fmt.Errorf("dhcp: SO_BINDTODEVICE %s: %w", cfg.BindDevice, err)}
			return
		}

		ch <- bound{conn: conn}
		r.serve(conn)
	}()

	b := <-ch
	if b.err != nil {
		return nil, b.err
	}
	r.conn = b.conn
	log.Info("dhcp responder started",
		"offered_ip", cfg.OfferedIP,
		"client_mac", macString(cfg.ClientMAC),
	)
	return r, nil
}

// Stop closes the listener, causing the serve goroutine to exit.
// Safe to call concurrently; safe to call more than once.
func (r *Responder) Stop() error {
	var err error
	r.stopOnce.Do(func() {
		close(r.stopped)
		if r.conn != nil {
			err = r.conn.Close()
		}
	})
	return err
}

// serve is the read loop. Runs on the netns-tainted goroutine
// that Start spawned. Exits when the listener is closed.
func (r *Responder) serve(conn net.PacketConn) {
	defer r.log.Info("dhcp responder stopped")

	buf := make([]byte, 2048)
	for {
		// 1s read deadline so Stop() doesn't block on a quiet
		// channel waiting for the read to wake up. The listener
		// close will still unblock us; the deadline is belt-and-
		// suspenders.
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, addr, err := conn.ReadFrom(buf)
		select {
		case <-r.stopped:
			return
		default:
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if isClosedError(err) {
				return
			}
			r.log.Debug("dhcp read error", "err", err)
			continue
		}
		r.handle(conn, addr, buf[:n])
	}
}

// handle processes one incoming DHCP packet.
func (r *Responder) handle(conn net.PacketConn, addr net.Addr, packet []byte) {
	m, err := Parse(packet)
	if err != nil {
		r.log.Debug("dhcp parse error", "err", err, "remote", addr)
		return
	}
	if m.Op != opBootRequest {
		return // only answer BOOTREQUEST, never reply to our own replies
	}
	// We intentionally don't filter by cfg.ClientMAC. The per-
	// sandbox netns + bridge already guarantees a single client
	// on this responder's wire; adding a MAC filter is defense-
	// in-depth that breaks forks, which restore from snapshot and
	// therefore carry the *source's* MAC in the guest's eth0 (MAC
	// is baked into snapshot state).
	if _, err := m.ClientMAC(); err != nil {
		r.log.Debug("dhcp unexpected HLen", "hlen", m.HLen)
		return
	}
	mt, ok := m.MessageType()
	if !ok {
		r.log.Debug("dhcp missing message type option")
		return
	}

	switch mt {
	case MsgDiscover:
		reply := r.buildReply(m, MsgOffer)
		r.sendBroadcast(conn, reply)
		r.log.Info("dhcp offered", "xid", fmt.Sprintf("%08x", m.XID))
	case MsgRequest:
		// Accept the request only if the client is asking for the
		// IP we're configured to hand out. For fork resume, the
		// guest will REQUEST the source's IP — we NAK it, dhclient
		// retries with DISCOVER, and the next round gets the right
		// address.
		requested, hasReq := m.RequestedIP()
		ciaddr := netip.AddrFrom4(m.CIAddr)
		accept := (hasReq && requested == r.cfg.OfferedIP) ||
			(!hasReq && ciaddr == r.cfg.OfferedIP)
		if accept {
			reply := r.buildReply(m, MsgAck)
			r.sendBroadcast(conn, reply)
			r.log.Info("dhcp ack", "xid", fmt.Sprintf("%08x", m.XID))
		} else {
			reply := r.buildReply(m, MsgNak)
			r.sendBroadcast(conn, reply)
			r.log.Info("dhcp nak",
				"xid", fmt.Sprintf("%08x", m.XID),
				"requested", requested.String(),
				"ciaddr", ciaddr.String(),
				"offered", r.cfg.OfferedIP.String(),
			)
		}
	default:
		// Ignore DECLINE / RELEASE / INFORM / INFORM for v0.1.
		r.log.Debug("dhcp ignored message type", "type", mt)
	}
}

// buildReply constructs an OFFER / ACK / NAK from a received
// request. Follows RFC 2131 Table 3: preserve XID, flags, GIAddr,
// CHAddr; fill in YIAddr (unless NAK); set SIAddr to our server
// identifier.
func (r *Responder) buildReply(req *Message, msgType uint8) *Message {
	m := &Message{
		Op:    opBootReply,
		HType: htypeEthernet,
		HLen:  hlenMAC,
		Hops:  0,
		XID:   req.XID,
		Secs:  0,
		Flags: req.Flags,
	}
	if msgType != MsgNak {
		yi := r.cfg.OfferedIP.As4()
		copy(m.YIAddr[:], yi[:])
	}
	serverIP := r.cfg.Gateway.As4() // server identifier = the gateway/host-side veth IP
	copy(m.SIAddr[:], serverIP[:])
	copy(m.CHAddr[:], req.CHAddr[:])
	copy(m.GIAddr[:], req.GIAddr[:])

	// Required options: message type + server ID.
	m.Options = append(m.Options,
		OptMsgType(msgType),
		OptServerID(r.cfg.Gateway),
	)
	if msgType == MsgOffer || msgType == MsgAck {
		m.Options = append(m.Options,
			OptLeaseTime(r.cfg.LeaseSeconds),
			OptSubnetMask(r.cfg.SubnetMask),
			OptRouter(r.cfg.Gateway),
			OptDNSServer(r.cfg.DNS),
		)
	}
	return m
}

// sendBroadcast writes the serialized reply to 255.255.255.255:68.
// The client hasn't configured its IP yet, so the standard DHCP
// practice is to broadcast replies; all clients on the L2 segment
// receive them but only the one whose XID matches processes it.
func (r *Responder) sendBroadcast(conn net.PacketConn, m *Message) {
	payload := Serialize(m)
	bcast := &net.UDPAddr{IP: net.IPv4(255, 255, 255, 255), Port: 68}
	if _, err := conn.WriteTo(payload, bcast); err != nil {
		r.log.Warn("dhcp write error", "err", err)
	}
}

// enableBroadcast sets SO_BROADCAST on the underlying UDP socket
// so WriteTo to 255.255.255.255 doesn't fail with EACCES.
func enableBroadcast(conn net.PacketConn) error {
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		return fmt.Errorf("dhcp: unexpected conn type %T", conn)
	}
	sc, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	cerr := sc.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
	})
	if cerr != nil {
		return cerr
	}
	return setErr
}

// bindToDevice pins the socket to a named L3 interface via
// SO_BINDTODEVICE. This scopes ingress *and* egress to that
// device and makes the kernel skip its routing-table lookup on
// send, which is what lets us broadcast to 255.255.255.255 inside
// a netns that has no default route. Requires CAP_NET_RAW (our
// daemon runs as root).
func bindToDevice(conn net.PacketConn, iface string) error {
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		return fmt.Errorf("dhcp: unexpected conn type %T", conn)
	}
	sc, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	cerr := sc.Control(func(fd uintptr) {
		setErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
	})
	if cerr != nil {
		return cerr
	}
	return setErr
}

// isClosedError recognizes the "use of closed network connection"
// error net/package returns after Close — we treat it as clean
// shutdown rather than a failure.
func isClosedError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed)
}

// macString formats a 6-byte MAC as colon-separated hex. Only used
// for logs.
func macString(mac [6]byte) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
}
