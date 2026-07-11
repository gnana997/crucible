package crucible

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// CreateApp creates a durable app (POST /apps): a named workload the daemon
// keeps a healthy instance of and re-creates from spec after a restart.
func (c *Client) CreateApp(ctx context.Context, req api.CreateAppRequest) (api.AppResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, "/apps", req)
	if err != nil {
		return api.AppResponse{}, err
	}
	return decodeInto[api.AppResponse](resp)
}

// ListApps returns every app with its observed status (GET /apps).
func (c *Client) ListApps(ctx context.Context) (Page[api.AppResponse], error) {
	resp, err := c.do(ctx, http.MethodGet, "/apps", nil)
	if err != nil {
		return Page[api.AppResponse]{}, err
	}
	out, err := decodeInto[api.AppListResponse](resp)
	return Page[api.AppResponse]{Items: out.Apps}, err
}

// GetApp fetches one app by name (GET /apps/{name}).
func (c *Client) GetApp(ctx context.Context, name string) (api.AppResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(name), nil)
	if err != nil {
		return api.AppResponse{}, err
	}
	return decodeInto[api.AppResponse](resp)
}

// UpdateApp replaces an app's spec (name immutable) and redeploys its instance
// from the new spec — the daemon bumps the app's generation and the reconciler
// destroys the old instance and boots a fresh one. Desired running/stopped is
// retained.
func (c *Client) UpdateApp(ctx context.Context, name string, spec api.AppSpec) (api.AppResponse, error) {
	resp, err := c.do(ctx, http.MethodPut, "/apps/"+url.PathEscape(name), spec)
	if err != nil {
		return api.AppResponse{}, err
	}
	return decodeInto[api.AppResponse](resp)
}

// DeleteApp removes an app and tears down its instance (DELETE /apps/{name}).
func (c *Client) DeleteApp(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/apps/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// App returns a handle for one app by name. Purely local until a call.
func (c *Client) App(name string) App { return App{Name: name, c: c} }

// App is a handle on one durable app. Exec/Logs transparently target the
// app's current instance.
type App struct {
	Name string
	c    *Client
}

// Get fetches the app's desired state plus observed status.
func (a App) Get(ctx context.Context) (api.AppResponse, error) {
	return a.c.GetApp(ctx, a.Name)
}

// Delete removes the app.
func (a App) Delete(ctx context.Context) error {
	return a.c.DeleteApp(ctx, a.Name)
}

// Exec runs a command in the app's current instance (POST /apps/{name}/exec);
// see Client.AppExec. The daemon resolves the instance per request, so this
// stays correct across a self-heal or rolling update. Errors 409 when the app
// has no running instance.
func (a App) Exec(ctx context.Context, req wire.ExecRequest, stdout, stderr io.Writer) (wire.ExecResult, error) {
	return a.c.AppExec(ctx, a.Name, req, stdout, stderr)
}

// Logs reads the current instance's durable logs (GET /apps/{name}/logs); see
// Client.AppLogs.
func (a App) Logs(ctx context.Context, since int64, source string) (api.LogsResponse, error) {
	return a.c.AppLogs(ctx, a.Name, since, source)
}
