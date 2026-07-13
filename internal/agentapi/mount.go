package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gnana997/crucible/sdk/wire"
)

// Mount asks the guest agent to mount a persistent volume's block device at a
// path (POST /mount). The daemon calls this after the agent is healthy and
// before starting the workload, so a volume-backed app (e.g. postgres) sees
// its data directory mounted before it launches. Idempotent on the agent side
// (a re-mount of the same target is a no-op), so it is safe to retry.
func (c *Client) Mount(ctx context.Context, spec wire.MountSpec) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/mount", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentapi: mount: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agentapi: mount %s at %s returned %d: %s",
			spec.Device, spec.Mountpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
