package crucible

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gnana997/crucible/sdk/api"
)

// Logs reads a sandbox's durable logs (GET /sandboxes/{id}/logs). A
// negative since tails the recent log; since >= 0 returns records after
// that byte offset (for a follow poll). source is "", "all", "service",
// or "exec". The response's NextOffset is the cursor for the next poll.
func (c *Client) Logs(ctx context.Context, id string, since int64, source string) (api.LogsResponse, error) {
	q := url.Values{}
	if since >= 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if source != "" && source != "all" {
		q.Set("source", source)
	}
	path := "/sandboxes/" + url.PathEscape(id) + "/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.LogsResponse{}, err
	}
	return decodeInto[api.LogsResponse](resp)
}
