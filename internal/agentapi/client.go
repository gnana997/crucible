package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// DefaultHandshakeTimeout bounds the CONNECT/OK exchange when the caller
// hasn't set a ctx deadline. 5s is comfortably longer than the sub-ms
// Firecracker takes, while still failing fast on a dead socket.
const DefaultHandshakeTimeout = 5 * time.Second

// maxHandshakeLine caps the OK-line length we'll read from Firecracker.
// The real response is ~12 bytes ("OK 1073741824\n"); this is defensive.
const maxHandshakeLine = 128

// maxRefreshBody caps how much of a guest agent's refresh response we
// buffer. Root-in-guest is in the threat model, so a compromised agent
// could otherwise stream a multi-GB body over vsock (GB/s) and OOM the
// host — the 15s ctx is not a meaningful cap. Real responses are a short
// error string at most. Mirrors exec.go's maxErrorBody.
const maxRefreshBody = 8 << 10

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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agentapi: network refresh returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// ErrIdentityRefreshUnsupported reports that the guest agent predates
// the /identity/refresh endpoint (HTTP 404) — the rootfs was built
// with an older crucible-agent. Callers must treat this as fatal:
// continuing would hand out forks with duplicated RNG state and
// identifiers. Fix: rebuild the rootfs with the current agent
// (scripts/build-rootfs.sh).
var ErrIdentityRefreshUnsupported = errors.New(
	"agentapi: guest agent does not support identity refresh — rebuild the rootfs with the current crucible-agent")

// RefreshIdentity asks the guest agent to give a freshly-forked VM
// unique state: credit the host-generated 32-byte seed to the kernel
// entropy pool (forcing a CRNG reseed), rotate /etc/machine-id, set
// the hostname to sandboxID, and write the /run/crucible/fork-id
// marker. Called by sandbox.Manager.Fork after resume, before the
// fork is registered — and thus before anything can exec into it.
// See docs/clone-safety.md.
//
// Unlike RefreshNetwork, failures ARE fatal to Fork: duplicated
// entropy doesn't self-heal on a renewal cycle the way a stale DHCP
// lease does. A 404 maps to ErrIdentityRefreshUnsupported so callers
// can surface the stale-rootfs cause directly.
func (c *Client) RefreshIdentity(ctx context.Context, seed []byte, sandboxID string) error {
	body, err := json.Marshal(agentwire.IdentityRefreshRequest{Seed: seed, SandboxID: sandboxID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/identity/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentapi: identity refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if resp.StatusCode == http.StatusNotFound {
		return ErrIdentityRefreshUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agentapi: identity refresh returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
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
