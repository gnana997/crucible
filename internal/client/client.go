// Package client is a typed Go client for the crucible daemon's REST
// API. It is the single place that knows how to speak to the daemon over
// HTTP, shared by the CLI today and — by design — the TUI, an MCP server,
// and the SDK later. Everything is expressed in internal/api wire types
// plus agentwire for the exec frame stream, so consumers never hand-roll
// requests or parse frames themselves.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
)

// DefaultAddr is the daemon's default listen address.
const DefaultAddr = "127.0.0.1:7878"

// ErrNotFound wraps a 404 from the daemon so callers can branch on it.
var ErrNotFound = errors.New("not found")

// Client talks to a crucible daemon over HTTP.
type Client struct {
	base  string
	token string
	http  *http.Client
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
func (c *Client) ListSandboxes(ctx context.Context) ([]api.SandboxResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/sandboxes", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.ListResponse](resp)
	return out.Sandboxes, err
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

// Snapshot captures a sandbox (POST /sandboxes/{id}/snapshot).
func (c *Client) Snapshot(ctx context.Context, sandboxID string) (api.SnapshotResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/snapshot", nil)
	if err != nil {
		return api.SnapshotResponse{}, err
	}
	return decodeInto[api.SnapshotResponse](resp)
}

// ListSnapshots returns all snapshots (GET /snapshots).
func (c *Client) ListSnapshots(ctx context.Context) ([]api.SnapshotResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/snapshots", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.SnapshotListResponse](resp)
	return out.Snapshots, err
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
func (c *Client) Fork(ctx context.Context, snapshotID string, count int) ([]api.SandboxResponse, error) {
	path := "/snapshots/" + snapshotID + "/fork"
	if count > 0 {
		path += "?count=" + strconv.Itoa(count)
	}
	resp, err := c.do(ctx, http.MethodPost, path, nil)
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

// Exec runs a command in a sandbox and streams its output. stdout/stderr
// frames are written to the given writers (nil discards); the returned
// ExecResult carries the exit code and usage. A validation failure before
// streaming comes back as an error; a failure mid-stream is delivered by
// the daemon as an exit frame with ExitCode -1 (see handleExecSandbox).
func (c *Client) Exec(ctx context.Context, sandboxID string, req agentwire.ExecRequest, stdout, stderr io.Writer) (agentwire.ExecResult, error) {
	var result agentwire.ExecResult
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/exec", req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return result, errorFrom(resp)
	}

	gotExit := false
	for {
		frame, err := agentwire.ReadFrame(resp.Body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return result, fmt.Errorf("read exec frame: %w", err)
		}
		switch frame.Type {
		case agentwire.FrameStdout:
			if stdout != nil {
				if _, err := stdout.Write(frame.Payload); err != nil {
					return result, err
				}
			}
		case agentwire.FrameStderr:
			if stderr != nil {
				if _, err := stderr.Write(frame.Payload); err != nil {
					return result, err
				}
			}
		case agentwire.FrameExit:
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

// errorFrom turns a non-2xx response into an error, preferring the
// daemon's {"error":...} message. 404 wraps ErrNotFound so callers can
// branch on it. The body is consumed.
func errorFrom(resp *http.Response) error {
	var e api.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	msg := e.Error
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	}
	return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, msg)
}
