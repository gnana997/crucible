package fcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// defaultRequestTimeout bounds any single Firecracker API call. Individual
// callers can shorten it by passing a context with an earlier deadline.
const defaultRequestTimeout = 10 * time.Second

// Client talks to a single Firecracker VMM over its unix-domain API socket.
//
// The only Firecracker-specific trick here is the transport: we use Go's
// stdlib net/http but override DialContext so that any HTTP request
// actually connects to a unix socket file on disk instead of a TCP host.
// The Host portion of outgoing URLs is irrelevant — our dialer ignores
// it — so we use a fixed "http://firecracker" base. This keeps the rest
// of the code in this package free of unix-socket concerns.
type Client struct {
	socketPath string
	http       *http.Client
}

// NewClient returns a Client wired to the given Firecracker API socket.
// The socket file must already exist; NewClient does not wait for it.
// Callers that need to wait can poll GetInstanceInfo.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
				// The API socket is local and cheap; one idle conn is plenty.
				MaxIdleConns:    1,
				IdleConnTimeout: 30 * time.Second,
			},
			Timeout: defaultRequestTimeout,
		},
	}
}

// SocketPath returns the unix-socket path this client dials. Exposed so
// callers (logs, error messages, tests) can report it.
func (c *Client) SocketPath() string { return c.socketPath }

// do issues one HTTP request to the Firecracker API.
//
//   - body, if non-nil, is JSON-encoded and sent with Content-Type: application/json.
//   - out, if non-nil, is filled from the JSON response body.
//   - 2xx responses return nil (or an out-decode error).
//   - Non-2xx responses are turned into a *Error by decodeError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("fcapi: marshal %s %s: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, bodyReader)
	if err != nil {
		return fmt.Errorf("fcapi: build request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fcapi: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if out == nil {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("fcapi: decode %s %s response: %w", method, path, err)
	}
	return nil
}
