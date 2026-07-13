package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gnana997/crucible/sdk/wire"
)

// ErrFreezeUnsupported reports that the guest agent predates the /freeze
// endpoint (HTTP 404). The caller cannot take a no-downtime live backup; rebuild
// the app on the current crucible-agent, or back up a slept/detached volume.
var ErrFreezeUnsupported = errors.New(
	"agentapi: guest agent does not support freeze — rebuild the app with the current crucible-agent")

// Freeze asks the guest agent to FIFREEZE one mounted filesystem (POST /freeze)
// so the host can copy its backing file consistently while the guest keeps
// running. The daemon MUST pair every Freeze with a Thaw (the agent also
// auto-thaws on a watchdog if the thaw is lost). A 404 maps to
// ErrFreezeUnsupported.
func (c *Client) Freeze(ctx context.Context, spec wire.FreezeSpec) error {
	return c.freezeOp(ctx, "freeze", spec)
}

// Thaw asks the guest agent to FITHAW a previously frozen filesystem
// (POST /thaw). Safe (and expected) to call in a defer after Freeze.
func (c *Client) Thaw(ctx context.Context, spec wire.FreezeSpec) error {
	return c.freezeOp(ctx, "thaw", spec)
}

func (c *Client) freezeOp(ctx context.Context, op string, spec wire.FreezeSpec) error {
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/"+op, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentapi: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if resp.StatusCode == http.StatusNotFound {
		return ErrFreezeUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agentapi: %s %s returned %d: %s",
			op, spec.Mountpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
