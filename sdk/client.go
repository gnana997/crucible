// Package crucible is the official Go SDK for the crucible daemon's REST
// API. It is the single place that knows how to speak to the daemon over
// HTTP — the in-repo CLI, TUI, and MCP server are thin adapters over it,
// and external programs import it the same way. Everything is expressed
// in the sdk/api wire types plus sdk/wire for the exec frame stream, so
// consumers never hand-roll requests or parse frames themselves.
//
// A daemon bearer token grants full control of that host's microVMs, so
// treat this as a server-side library: never embed a token in code that
// ships to a browser or an untrusted client.
package crucible

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// DefaultAddr is the daemon's default listen address.
const DefaultAddr = "127.0.0.1:7878"

// Client talks to a crucible daemon over HTTP.
type Client struct {
	base     string
	token    string
	http     *http.Client
	insecure bool // skip TLS verification on raw (hijacked) dials
}

// Option configures a Client.
type Option func(*Client)

// WithToken sends "Authorization: Bearer <token>" on every request. Empty
// token is a no-op (unauthenticated, for a loopback daemon).
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithInsecureSkipVerify disables TLS certificate verification — for a
// self-signed daemon in development only. Never use against production.
func WithInsecureSkipVerify() Option {
	return func(c *Client) {
		c.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		c.insecure = true
	}
}

// New returns a Client for the given daemon address. addr may be a
// "host:port" or a full "http(s)://host:port" URL; empty means DefaultAddr.
func New(addr string, opts ...Option) *Client {
	if addr == "" {
		addr = DefaultAddr
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	c := &Client{base: strings.TrimRight(addr, "/"), http: &http.Client{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Health reports whether the daemon is serving (GET /healthz).
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/healthz", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errorFrom(resp)
	}
	return nil
}

// CreateSandbox creates a sandbox (POST /sandboxes).
func (c *Client) CreateSandbox(ctx context.Context, req api.CreateSandboxRequest) (api.SandboxResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes", req)
	if err != nil {
		return api.SandboxResponse{}, err
	}
	return decodeInto[api.SandboxResponse](resp)
}

// ListSandboxes returns all live sandboxes (GET /sandboxes).
func (c *Client) ListSandboxes(ctx context.Context) (Page[api.SandboxResponse], error) {
	resp, err := c.do(ctx, http.MethodGet, "/sandboxes", nil)
	if err != nil {
		return Page[api.SandboxResponse]{}, err
	}
	out, err := decodeInto[api.ListResponse](resp)
	return Page[api.SandboxResponse]{Items: out.Sandboxes}, err
}

// GetSandbox fetches one sandbox (GET /sandboxes/{id}).
func (c *Client) GetSandbox(ctx context.Context, id string) (api.SandboxResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/sandboxes/"+id, nil)
	if err != nil {
		return api.SandboxResponse{}, err
	}
	return decodeInto[api.SandboxResponse](resp)
}

// DeleteSandbox destroys a sandbox (DELETE /sandboxes/{id}).
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/sandboxes/"+id, nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// CopyTo streams a tar archive into a sandbox (POST /sandboxes/{id}/files),
// extracted beneath dest (an absolute guest directory). The tar reader is sent
// as the raw request body, so a large project streams without being buffered.
func (c *Client) CopyTo(ctx context.Context, sandboxID, dest string, tar io.Reader) (wire.FilesPutResult, error) {
	var res wire.FilesPutResult
	u := c.base + "/sandboxes/" + url.PathEscape(sandboxID) + "/files?path=" + url.QueryEscape(dest)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, tar)
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return res, fmt.Errorf("connect to daemon at %s: %w", c.base, err)
	}
	return decodeInto[wire.FilesPutResult](resp)
}

// ReadFile reads a single file at path inside a sandbox
// (GET /sandboxes/{id}/files?path=…) and returns its bytes, capped at maxBytes.
// Only file content flows back; nothing is written on the host.
func (c *Client) ReadFile(ctx context.Context, sandboxID, path string, maxBytes int) ([]byte, error) {
	u := c.base + "/sandboxes/" + url.PathEscape(sandboxID) + "/files?path=" + url.QueryEscape(path)
	if maxBytes > 0 {
		u += "&max_bytes=" + strconv.Itoa(maxBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon at %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, errorFrom(resp)
	}
	limit := int64(1 << 30)
	if maxBytes > 0 {
		limit = int64(maxBytes)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Snapshot captures a sandbox (POST /sandboxes/{id}/snapshot).
func (c *Client) Snapshot(ctx context.Context, sandboxID string) (api.SnapshotResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/snapshot", nil)
	if err != nil {
		return api.SnapshotResponse{}, err
	}
	return decodeInto[api.SnapshotResponse](resp)
}

// ListSnapshots returns all snapshots (GET /snapshots).
func (c *Client) ListSnapshots(ctx context.Context) (Page[api.SnapshotResponse], error) {
	resp, err := c.do(ctx, http.MethodGet, "/snapshots", nil)
	if err != nil {
		return Page[api.SnapshotResponse]{}, err
	}
	out, err := decodeInto[api.SnapshotListResponse](resp)
	return Page[api.SnapshotResponse]{Items: out.Snapshots}, err
}

// GetSnapshot fetches one snapshot (GET /snapshots/{id}).
func (c *Client) GetSnapshot(ctx context.Context, id string) (api.SnapshotResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/snapshots/"+id, nil)
	if err != nil {
		return api.SnapshotResponse{}, err
	}
	return decodeInto[api.SnapshotResponse](resp)
}

// DeleteSnapshot deletes a snapshot (DELETE /snapshots/{id}).
func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/snapshots/"+id, nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// Fork creates count sandboxes from a snapshot (POST /snapshots/{id}/fork).
// Optional publish mappings expose the fork's guest ports on the host
// (`docker run -p` semantics); publishing requires count 1 and a daemon
// >= v0.3.4 (older daemons ignore the request body).
func (c *Client) Fork(ctx context.Context, snapshotID string, count int, publish ...api.PortMapping) ([]api.SandboxResponse, error) {
	path := "/snapshots/" + snapshotID + "/fork"
	var body any
	if len(publish) > 0 {
		body = api.ForkRequest{Count: count, Publish: publish}
	} else if count > 0 {
		// Query-only form: understood by every daemon version.
		path += "?count=" + strconv.Itoa(count)
	}
	resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.ForkResponse](resp)
	return out.Sandboxes, err
}

// ListProfiles returns the daemon's configured rootfs profiles (GET /profiles).
func (c *Client) ListProfiles(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/profiles", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.ProfilesResponse](resp)
	return out.Profiles, err
}

// Whoami returns what the daemon knows about this client's credential
// (GET /whoami): whether it is scoped, and if so its effective policy.
func (c *Client) Whoami(ctx context.Context) (Identity, error) {
	resp, err := c.do(ctx, http.MethodGet, "/whoami", nil)
	if err != nil {
		return Identity{}, err
	}
	return decodeInto[Identity](resp)
}

// Exec runs a command in a sandbox and streams its output. stdout/stderr
// frames are written to the given writers (nil discards); the returned
// ExecResult carries the exit code and usage. A validation failure before
// streaming comes back as an error; a failure mid-stream is delivered by
// the daemon as an exit frame with ExitCode -1 (see handleExecSandbox).
func (c *Client) Exec(ctx context.Context, sandboxID string, req wire.ExecRequest, stdout, stderr io.Writer) (wire.ExecResult, error) {
	return c.execStream(ctx, "/sandboxes/"+url.PathEscape(sandboxID)+"/exec", req, stdout, stderr)
}

// AppExec runs a command in an app's CURRENT instance (POST /apps/{name}/exec).
// The daemon resolves the app to its instance server-side per request, so it
// targets whatever instance is current across a self-heal or rolling update —
// never a stale id captured beforehand. Errors 409 when the app has no
// running instance.
func (c *Client) AppExec(ctx context.Context, appName string, req wire.ExecRequest, stdout, stderr io.Writer) (wire.ExecResult, error) {
	return c.execStream(ctx, "/apps/"+url.PathEscape(appName)+"/exec", req, stdout, stderr)
}

func (c *Client) execStream(ctx context.Context, path string, req wire.ExecRequest, stdout, stderr io.Writer) (wire.ExecResult, error) {
	var result wire.ExecResult
	resp, err := c.do(ctx, http.MethodPost, path, req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return result, errorFrom(resp)
	}

	gotExit := false
	for {
		frame, err := wire.ReadFrame(resp.Body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return result, fmt.Errorf("read exec frame: %w", err)
		}
		switch frame.Type {
		case wire.FrameStdout:
			if stdout != nil {
				if _, err := stdout.Write(frame.Payload); err != nil {
					return result, err
				}
			}
		case wire.FrameStderr:
			if stderr != nil {
				if _, err := stderr.Write(frame.Payload); err != nil {
					return result, err
				}
			}
		case wire.FrameExit:
			if err := json.Unmarshal(frame.Payload, &result); err != nil {
				return result, fmt.Errorf("decode exit frame: %w", err)
			}
			gotExit = true
		}
	}
	if !gotExit {
		return result, errors.New("exec stream ended without an exit frame")
	}
	return result, nil
}

// ExecInteractive runs a command in a sandbox as a full-duplex interactive
// session (a live shell). It dials the daemon on a raw connection, sends
// POST /sandboxes/{id}/exec?stdin=1 with the JSON ExecRequest, then owns the
// conn: it streams stdin as FrameStdin frames and reads FrameStdout /
// FrameStderr / FrameExit back. stdin is read to EOF, at which point a
// FrameStdinClose is sent. The returned ExecResult carries the exit code.
//
// There is no PTY — this is a functional shell (line-buffered, no raw mode
// or terminal control), suitable for exploring a running sandbox.
func (c *Client) ExecInteractive(ctx context.Context, sandboxID string, req wire.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (wire.ExecResult, error) {
	return c.execInteractive(ctx, "/sandboxes/"+url.PathEscape(sandboxID)+"/exec?stdin=1", req, stdin, stdout, stderr)
}

// AppExecInteractive is ExecInteractive against an app's CURRENT instance
// (POST /apps/{name}/exec?stdin=1), resolved server-side per request.
func (c *Client) AppExecInteractive(ctx context.Context, appName string, req wire.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (wire.ExecResult, error) {
	return c.execInteractive(ctx, "/apps/"+url.PathEscape(appName)+"/exec?stdin=1", req, stdin, stdout, stderr)
}

func (c *Client) execInteractive(ctx context.Context, reqPath string, req wire.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (wire.ExecResult, error) {
	var result wire.ExecResult

	conn, host, err := c.dialRaw(ctx)
	if err != nil {
		return result, err
	}
	defer func() { _ = conn.Close() }()

	// Close the conn if ctx is cancelled (Ctrl-C) so the read loop below
	// unblocks and returns promptly; the daemon sees the conn drop and kills
	// the guest process. The done channel stops this watcher on the normal
	// (uncancelled) exit so it never leaks.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	body, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	var hdr bytes.Buffer
	fmt.Fprintf(&hdr, "POST %s HTTP/1.1\r\n", reqPath)
	fmt.Fprintf(&hdr, "Host: %s\r\n", host)
	hdr.WriteString("Content-Type: application/json\r\n")
	fmt.Fprintf(&hdr, "Content-Length: %d\r\n", len(body))
	// Connection: close so a pre-stream error response (which the server
	// does not hijack) ends with EOF rather than lingering on keep-alive.
	hdr.WriteString("Connection: close\r\n")
	if c.token != "" {
		fmt.Fprintf(&hdr, "Authorization: Bearer %s\r\n", c.token)
	}
	hdr.WriteString("\r\n")
	hdr.Write(body)
	if _, err := conn.Write(hdr.Bytes()); err != nil {
		return result, fmt.Errorf("write exec request: %w", err)
	}

	status, err := readStatusCode(conn)
	if err != nil {
		return result, fmt.Errorf("read exec response: %w", err)
	}
	if status != http.StatusOK {
		// Bound the error-body read so a misbehaving daemon can't wedge us.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		msg, _ := io.ReadAll(io.LimitReader(conn, 8<<10))
		return result, fmt.Errorf("daemon returned %d: %s", status, strings.TrimSpace(string(msg)))
	}

	// Pump stdin → frames. On stdin EOF/error, send FrameStdinClose so the
	// guest process sees its stdin closed without dropping the connection.
	fw := wire.NewFrameWriter(conn)
	if stdin != nil {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, rerr := stdin.Read(buf)
				if n > 0 {
					if werr := fw.WriteFrame(wire.FrameStdin, buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					_ = fw.WriteFrame(wire.FrameStdinClose, nil)
					return
				}
			}
		}()
	}

	gotExit := false
	for {
		frame, err := wire.ReadFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return result, fmt.Errorf("read exec frame: %w", err)
		}
		switch frame.Type {
		case wire.FrameStdout:
			if stdout != nil {
				if _, err := stdout.Write(frame.Payload); err != nil {
					return result, err
				}
			}
		case wire.FrameStderr:
			if stderr != nil {
				if _, err := stderr.Write(frame.Payload); err != nil {
					return result, err
				}
			}
		case wire.FrameExit:
			if err := json.Unmarshal(frame.Payload, &result); err != nil {
				return result, fmt.Errorf("decode exit frame: %w", err)
			}
			gotExit = true
		}
	}
	if !gotExit {
		return result, errors.New("exec stream ended without an exit frame")
	}
	return result, nil
}

// --- internals -------------------------------------------------------

// dialRaw opens a raw TCP (or TLS) connection to the daemon for a hijacked
// full-duplex stream, returning the conn and the Host header value to use.
// net/http can't be used for the interactive exec path: its client request
// body is buffered, so per-keystroke stdin would not flush.
func (c *Client) dialRaw(ctx context.Context) (net.Conn, string, error) {
	u, err := url.Parse(c.base)
	if err != nil {
		return nil, "", fmt.Errorf("parse daemon address %q: %w", c.base, err)
	}
	host := u.Host
	if u.Scheme == "https" {
		d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: c.insecure, ServerName: u.Hostname()}}
		conn, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			return nil, "", fmt.Errorf("connect to daemon at %s: %w", c.base, err)
		}
		return conn, host, nil
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, "", fmt.Errorf("connect to daemon at %s: %w", c.base, err)
	}
	return conn, host, nil
}

// maxHeaderLine caps a single status/header line read while parsing the
// interactive-exec response. Real lines are tiny; this is defensive.
const maxHeaderLine = 8 << 10

// readStatusCode reads an HTTP/1.1 status line and headers one byte at a
// time — never over-reading into the frame stream that follows — and
// returns the numeric status code.
func readStatusCode(r io.Reader) (int, error) {
	statusLine, err := readCRLFLine(r)
	if err != nil {
		return 0, err
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0, fmt.Errorf("malformed status line %q", statusLine)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("malformed status code %q", parts[1])
	}
	for {
		line, err := readCRLFLine(r)
		if err != nil {
			return 0, err
		}
		if line == "" {
			break
		}
	}
	return code, nil
}

// readCRLFLine reads until '\n', returning the line without a trailing
// '\r'. Byte-at-a-time so no bytes past the line are consumed.
func readCRLFLine(r io.Reader) (string, error) {
	buf := make([]byte, 0, 64)
	var one [1]byte
	for len(buf) < maxHeaderLine {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return strings.TrimSuffix(string(buf), "\r"), nil
		}
		buf = append(buf, one[0])
	}
	return "", errors.New("header line exceeded maximum length")
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon at %s: %w", c.base, err)
	}
	return resp, nil
}

func decodeInto[T any](resp *http.Response) (T, error) {
	var out T
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return out, errorFrom(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func expectNoContent(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return errorFrom(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// errorFrom turns a non-2xx response into an *Error, preferring the
// daemon's {"error":...} message. The body is consumed. See errors.go
// for the sentinel mapping (errors.Is(err, ErrNotFound) etc.).
func errorFrom(resp *http.Response) error {
	var e api.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	msg := e.Error
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &Error{Status: resp.StatusCode, Code: e.Code, Message: msg}
}
