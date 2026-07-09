package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gnana997/crucible/internal/agentwire"
)

// PushFiles streams a tar archive to the guest agent's PUT /files handler,
// which extracts it beneath dest (an absolute directory inside the guest).
// The body is streamed straight from r, so nothing is buffered whole.
//
// Returns the agent's summary (files written, bytes).
func (c *Client) PushFiles(ctx context.Context, dest string, tar io.Reader) (agentwire.FilesPutResult, error) {
	var res agentwire.FilesPutResult
	u := "http://agent/files?path=" + url.QueryEscape(dest)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, tar)
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/x-tar")

	resp, err := c.http.Do(req)
	if err != nil {
		return res, fmt.Errorf("agentapi: files: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return res, fmt.Errorf("agentapi: files returned %d: %s", resp.StatusCode, string(msg))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&res); err != nil {
		return res, fmt.Errorf("agentapi: files decode: %w", err)
	}
	return res, nil
}

// ReadFile reads a single file at path inside the guest via GET /files and
// returns its bytes, capped at maxBytes. Only content flows out; nothing is
// written on the caller's side.
func (c *Client) ReadFile(ctx context.Context, path string, maxBytes int) ([]byte, error) {
	u := "http://agent/files?path=" + url.QueryEscape(path)
	if maxBytes > 0 {
		u += "&max_bytes=" + strconv.Itoa(maxBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentapi: files read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, fmt.Errorf("agentapi: files read returned %d: %s", resp.StatusCode, string(msg))
	}
	limit := int64(defaultReadCap)
	if maxBytes > 0 {
		limit = int64(maxBytes)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// defaultReadCap bounds a ReadFile response when the caller passes maxBytes<=0.
const defaultReadCap = 10 << 20
