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

// ListDomains returns the custom domains attached to an app
// (GET /apps/{name}/domains).
func (c *Client) ListDomains(ctx context.Context, name string) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(name)+"/domains", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodeInto[api.DomainListResponse](resp)
	return out.Domains, err
}

// AddDomain attaches a custom domain (FQDN, globally unique) to an app
// (POST /apps/{name}/domains), returning the updated app.
func (c *Client) AddDomain(ctx context.Context, name, domain string) (api.AppResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, "/apps/"+url.PathEscape(name)+"/domains", api.AddDomainRequest{Domain: domain})
	if err != nil {
		return api.AppResponse{}, err
	}
	return decodeInto[api.AppResponse](resp)
}

// RemoveDomain detaches a custom domain from an app
// (DELETE /apps/{name}/domains/{domain}).
func (c *Client) RemoveDomain(ctx context.Context, name, domain string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/apps/"+url.PathEscape(name)+"/domains/"+url.PathEscape(domain), nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// SleepApp snapshots the app's current instance and stops its VMM to free RAM
// (scale-to-zero, POST /apps/{name}/sleep). The app stays addressable and wakes
// on the next WakeApp. Errors 409 when the app has no running instance.
func (c *Client) SleepApp(ctx context.Context, name string) (api.AppResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, "/apps/"+url.PathEscape(name)+"/sleep", nil)
	if err != nil {
		return api.AppResponse{}, err
	}
	return decodeInto[api.AppResponse](resp)
}

// WakeApp restores a slept app's instance in place — same id, netns, and IP —
// reseeding its RNG and stepping its clock (POST /apps/{name}/wake). The
// returned status carries last_wake_latency_ms. Errors 409 when the app is not
// asleep.
func (c *Client) WakeApp(ctx context.Context, name string) (api.AppResponse, error) {
	resp, err := c.do(ctx, http.MethodPost, "/apps/"+url.PathEscape(name)+"/wake", nil)
	if err != nil {
		return api.AppResponse{}, err
	}
	return decodeInto[api.AppResponse](resp)
}

// Usage returns every app's persistent usage metrics plus the reading's
// snapshot time (GET /usage). Values are cumulative — diff two reads to bill.
func (c *Client) Usage(ctx context.Context) (api.UsageListResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/usage", nil)
	if err != nil {
		return api.UsageListResponse{}, err
	}
	return decodeInto[api.UsageListResponse](resp)
}

// AppUsage returns one app's persistent usage metrics by name, accrued to now
// (GET /apps/{name}/usage).
func (c *Client) AppUsage(ctx context.Context, name string) (api.AppUsage, error) {
	resp, err := c.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(name)+"/usage", nil)
	if err != nil {
		return api.AppUsage{}, err
	}
	return decodeInto[api.AppUsage](resp)
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

// Sleep snapshots the app and frees its RAM (scale-to-zero); see Client.SleepApp.
func (a App) Sleep(ctx context.Context) (api.AppResponse, error) {
	return a.c.SleepApp(ctx, a.Name)
}

// Wake restores a slept app in place; see Client.WakeApp.
func (a App) Wake(ctx context.Context) (api.AppResponse, error) {
	return a.c.WakeApp(ctx, a.Name)
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
