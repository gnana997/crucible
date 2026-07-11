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
	return c.logsAt(ctx, "/sandboxes/"+url.PathEscape(id)+"/logs", since, source)
}

// AppLogs reads an app's CURRENT instance's durable logs (GET /apps/{name}/logs),
// resolved to the current instance server-side per request. Same since/source
// semantics as Logs.
func (c *Client) AppLogs(ctx context.Context, appName string, since int64, source string) (api.LogsResponse, error) {
	return c.logsAt(ctx, "/apps/"+url.PathEscape(appName)+"/logs", since, source)
}

func (c *Client) logsAt(ctx context.Context, basePath string, since int64, source string) (api.LogsResponse, error) {
	q := url.Values{}
	if since >= 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if source != "" && source != "all" {
		q.Set("source", source)
	}
	path := basePath
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.LogsResponse{}, err
	}
	return decodeInto[api.LogsResponse](resp)
}
