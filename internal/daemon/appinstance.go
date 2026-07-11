package daemon

import (
	"context"
	"errors"
	"fmt"

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
