package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/api"
)

// appInstantiator adapts the daemon's sandbox Manager to app.Instantiator:
// it boots an app's instance through the exact same buildCreateConfig path
// a `create --image` request uses, so an app inherits image resolution,
// networking, publish, and the durable-log drain with no divergence.
type appInstantiator struct{ s *Server }

// Create boots a fresh instance from the app spec and returns its sandbox
// id. App instances carry no lifetime timeout (they are long-lived; the
// AppSpec has no timeout field), so nothing kills them out from under the
// reconciler.
func (a appInstantiator) Create(ctx context.Context, appID string, spec api.AppSpec) (string, error) {
	req := api.CreateSandboxRequest{
		Image:     spec.Image,
		Pull:      spec.Pull,
		VCPUs:     spec.VCPUs,
		MemoryMiB: spec.MemoryMiB,
		DiskBytes: spec.DiskBytes,
		Network:   spec.Network,
		Publish:   spec.Publish,
		Service:   spec.Service,
	}
	pull, err := validatePull(req.Pull)
	if err != nil {
		return "", err
	}
	cfg, ierr := a.s.buildCreateConfig(ctx, &req, pull)
	if ierr != nil {
		return "", ierr.err
	}
	// App env applies to the entrypoint the guest supervisor runs, so it
	// merges onto the effective service (app values win). An app with env
	// but no entrypoint has nowhere to put it — silently ignored.
	if len(spec.Env) > 0 && cfg.Service != nil {
		merged := make(map[string]string, len(cfg.Service.Env)+len(spec.Env))
		for k, v := range cfg.Service.Env {
			merged[k] = v
		}
		for k, v := range spec.Env {
			merged[k] = v
		}
		cfg.Service.Env = merged
	}

	sb, err := a.s.cfg.Manager.Create(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("app %s: create instance: %w", appID, err)
	}
	if cfg.Service != nil && a.s.cfg.LogStore != nil {
		a.s.startServiceDrain(sb.ID)
	}
	return sb.ID, nil
}

// Exists reports whether the instance is still registered in the Manager.
func (a appInstantiator) Exists(instanceID string) bool {
	_, err := a.s.cfg.Manager.Get(instanceID)
	return err == nil
}

// Probe runs the app's health check against the instance's guest. http and
// tcp dial the guest IP directly (reachable from the daemon's root netns
// via the per-sandbox veth, the same path the port-publish forwarder
// uses). exec probes — and image-HEALTHCHECK seeding, since Docker's
// HEALTHCHECK is always a command — are a follow-up: they need in-guest
// command exec as a probe, so exec returns HealthUnknown for now.
func (a appInstantiator) Probe(ctx context.Context, instanceID string, hc api.HealthCheck) app.Health {
	sb, err := a.s.cfg.Manager.Get(instanceID)
	if err != nil || sb.Network == nil || sb.Network.GuestIP == "" {
		return app.HealthUnknown
	}
	addr := net.JoinHostPort(sb.Network.GuestIP, strconv.Itoa(hc.Port))
	timeout := time.Duration(hc.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	switch hc.Type {
	case "tcp":
		d := net.Dialer{Timeout: timeout}
		conn, derr := d.DialContext(ctx, "tcp", addr)
		if derr != nil {
			return app.HealthFailing
		}
		_ = conn.Close()
		return app.HealthPassing

	case "http":
		path := hc.Path
		if path == "" {
			path = "/"
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, rerr := http.NewRequestWithContext(pctx, http.MethodGet, "http://"+addr+path, nil)
		if rerr != nil {
			return app.HealthUnknown
		}
		// A one-shot client with no keep-alive: probes shouldn't pool.
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		resp, herr := client.Do(req)
		if herr != nil {
			return app.HealthFailing
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return app.HealthPassing
		}
		return app.HealthFailing

	default:
		return app.HealthUnknown
	}
}

// Destroy tears the instance down. A not-found instance is already gone,
// which is success from the reconciler's view.
func (a appInstantiator) Destroy(ctx context.Context, instanceID string) error {
	err := a.s.cfg.Manager.Delete(ctx, instanceID)
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil
	}
	return err
}

// NewAppInstantiator returns the daemon's app.Instantiator. Exposed so the
// process wiring (cmd/crucible) can construct the app manager.
func (s *Server) NewAppInstantiator() app.Instantiator { return appInstantiator{s: s} }
