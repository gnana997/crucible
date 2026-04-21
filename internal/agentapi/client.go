package agentapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultHandshakeTimeout bounds the CONNECT/OK exchange when the caller
// hasn't set a ctx deadline. 5s is comfortably longer than the sub-ms
// Firecracker takes, while still failing fast on a dead socket.
const DefaultHandshakeTimeout = 5 * time.Second

// maxHandshakeLine caps the OK-line length we'll read from Firecracker.
// The real response is ~12 bytes ("OK 1073741824\n"); this is defensive.
const maxHandshakeLine = 128

// Client talks HTTP to a single sandbox's guest agent over Firecracker's
// hybrid-vsock unix socket. One Client per sandbox.
type Client struct {
	udsPath string
	port    uint32
	http    *http.Client
}

// NewClient wires a Client to the given host UDS path (Firecracker
// creates this when you PUT /vsock) and the guest-side vsock port the
// agent listens on (agentwire.AgentVSockPort for crucible).
//
// Callers set per-request deadlines via ctx; the Client itself has no
// global timeout because /exec can stream for as long as the command
// takes.
func NewClient(udsPath string, port uint32) *Client {
	c := &Client{udsPath: udsPath, port: port}
	c.http = &http.Client{
		Transport: &http.Transport{
			DialContext: c.dial,
			// Reuse a single idle connection per sandbox — healthz
			// followed by exec is the common pattern.
			MaxIdleConns:    1,
			IdleConnTimeout: 30 * time.Second,
		},
	}
	return c
}

// SocketPath returns the host UDS path this Client dials. Exposed for
// logs, errors, and tests.
func (c *Client) SocketPath() string { return c.udsPath }

// GetHealthz polls GET /healthz on the guest agent. Returns nil if the
// agent is up and answering; useful as a readiness gate after a
// sandbox boots.
func (c *Client) GetHealthz(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentapi: healthz: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agentapi: healthz returned %d", resp.StatusCode)
	}
	return nil
}

// RefreshNetwork asks the guest agent to bounce eth0 so
// systemd-networkd runs a fresh DHCP cycle. Called by
// sandbox.Manager.Fork immediately after the fork VM resumes —
// the guest's eth0 restored-from-snapshot holds the *source's*
// IP, and we want the fork's assigned IP in place without
// waiting for a renewal-timer cycle.
//
// Returns nil on success. Non-success cases the caller should
// handle but not treat as fatal to Fork:
//
//   - Agent unreachable (VM still booting, vsock not ready):
//     surfaces as a dial error. Log and continue; the guest will
//     eventually recover via systemd-networkd's renewal cycle.
//   - Agent reports 500: body identifies which step failed
//     (down / up / wait). Typical causes: rootfs missing the
//     netplan eth0-DHCP config, or the per-netns DHCP responder
//     never answered within the wait timeout.
func (c *Client) RefreshNetwork(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/network/refresh", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentapi: network refresh: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agentapi: network refresh returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// dial is http.Transport.DialContext: open the UDS, do the hybrid-vsock
// handshake, return the stream for HTTP to use. All errors mean the
// caller gets a dial-level failure — http.Client won't retry HTTP-level
// semantics on top of them.
func (c *Client) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.udsPath)
	if err != nil {
		return nil, fmt.Errorf("agentapi: dial %s: %w", c.udsPath, err)
	}

	// Bound the handshake independently of any later HTTP deadline: if
	// the caller's ctx has no deadline, fall back to a short default
	// (Firecracker answers in well under a ms).
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(DefaultHandshakeTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", c.port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("agentapi: write CONNECT: %w", err)
	}

	line, err := readHandshakeLine(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("agentapi: read handshake: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		_ = conn.Close()
		return nil, fmt.Errorf("agentapi: handshake: expected %q, got %q", "OK ...", line)
	}

	// Clear the deadline so the subsequent HTTP traffic isn't capped by
	// the handshake deadline. HTTP-level cancellation still works via
	// the request context.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// readHandshakeLine reads a single '\n'-terminated line from r one byte
// at a time. Deliberately NOT bufio: bufio.Reader would read ahead and
// swallow bytes that belong to the HTTP response that follows on the
// same connection.
func readHandshakeLine(r io.Reader) (string, error) {
	buf := make([]byte, 0, 16)
	var one [1]byte
	for i := 0; i < maxHandshakeLine; i++ {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(buf), nil
		}
		buf = append(buf, one[0])
	}
	return "", errors.New("handshake line exceeded maximum length")
}
