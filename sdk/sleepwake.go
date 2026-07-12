package crucible

import (
	"context"
	"net/http"
	"net/url"
)

// SleepSandbox snapshots a sandbox and stops its VMM to free RAM (scale-to-zero,
// POST /sandboxes/{id}/sleep). This is the low-level primitive; the app-level
// SleepApp is the product surface. Errors 404 for an unknown sandbox.
func (c *Client) SleepSandbox(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/sleep", nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// WakeSandbox restores a slept sandbox in place — same id, netns, and IP —
// reseeding its RNG and stepping its clock (POST /sandboxes/{id}/wake).
func (c *Client) WakeSandbox(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/wake", nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}
