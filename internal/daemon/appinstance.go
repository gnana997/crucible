package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/internal/network"
	"github.com/gnana997/crucible/internal/oci"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
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
		Image:      spec.Image,
		Pull:       spec.Pull,
		VCPUs:      spec.VCPUs,
		MemoryMiB:  spec.MemoryMiB,
		DiskBytes:  spec.DiskBytes,
		Network:    spec.Network,
		Publish:    spec.Publish,
		PublishAll: spec.PublishAll,
		Service:    spec.Service,
	}
	pull, err := validatePull(req.Pull)
	if err != nil {
		return "", err
	}
	cfg, ierr := a.s.buildCreateConfig(ctx, &req, pull)
	if ierr != nil {
		return "", ierr.err
	}
	// A proxied app (Port set) needs a NIC so the ingress proxy can reach the
	// guest over its veth — even with no published ports or egress. Synthesize a
	// deny-all network (ingress-reachable, egress-denied), mirroring the publish
	// path in buildCreateConfig.
	if cfg.Network == nil && spec.Port > 0 {
		denyAll, derr := network.New(nil)
		if derr != nil {
			return "", fmt.Errorf("app %s: deny-all network: %w", appID, derr)
		}
		cfg.Network = &sandbox.NetworkConfig{Allowlist: denyAll}
	}
	// App env applies to the entrypoint the guest supervisor runs, so it
	// merges onto the effective service (app values win). An app with env
	// but no entrypoint has nowhere to put it — silently ignored.
	mergeAppEnv(cfg.Service, spec.Env)

	sb, err := a.s.cfg.Manager.Create(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("app %s: create instance: %w", appID, err)
	}
	if cfg.Service != nil && a.s.cfg.LogStore != nil {
		a.s.startServiceDrain(sb.ID)
	}
	return sb.ID, nil
}

// mergeAppEnv overlays an app's env onto the effective service env, app values
// winning over the image's ENV. A no-op when there is no app env or no service
// (an app env with no entrypoint has nowhere to land). Mutates svc in place.
func mergeAppEnv(svc *wire.ServiceSpec, appEnv map[string]string) {
	if len(appEnv) == 0 || svc == nil {
		return
	}
	merged := make(map[string]string, len(svc.Env)+len(appEnv))
	for k, v := range svc.Env {
		merged[k] = v
	}
	for k, v := range appEnv {
		merged[k] = v
	}
	svc.Env = merged
}

// ImageHealth resolves the app's image (store-hit or a one-time pull) and
// returns the health check derived from its Docker HEALTHCHECK, or nil when the
// image declares none / NONE. The reconciler uses it to seed an app that
// declares no health of its own.
func (a appInstantiator) ImageHealth(ctx context.Context, spec api.AppSpec) (*api.HealthCheck, error) {
	if spec.Image == nil || spec.Image.OCI == "" {
		return nil, nil
	}
	// Apps use the persistent credential store (they re-pull on restart with no
	// request present), so no per-request auth here.
	rec, _, ierr := a.s.resolveImage(ctx, spec.Image, pullMissing, nil)
	if ierr != nil {
		return nil, ierr.err
	}
	if rec == nil || rec.RunConfig == nil {
		return nil, nil
	}
	return healthFromImage(rec.RunConfig.Healthcheck), nil
}

// healthFromImage converts a Docker HEALTHCHECK (oci.Healthcheck) into an
// app.HealthCheck. Test[0] selects the form: CMD → exec argv; CMD-SHELL →
// /bin/sh -c; NONE (or absent/unknown) → nil (no seed).
func healthFromImage(hc *oci.Healthcheck) *api.HealthCheck {
	if hc == nil || len(hc.Test) == 0 {
		return nil
	}
	var cmd []string
	switch hc.Test[0] {
	case "CMD":
		cmd = append([]string(nil), hc.Test[1:]...)
	case "CMD-SHELL":
		if len(hc.Test) < 2 {
			return nil
		}
		cmd = []string{"/bin/sh", "-c", hc.Test[1]}
	default: // "NONE" or an unrecognized form
		return nil
	}
	if len(cmd) == 0 {
		return nil
	}
	out := &api.HealthCheck{Type: "exec", Cmd: cmd}
	if hc.IntervalMs > 0 {
		out.IntervalSec = int(hc.IntervalMs / 1000)
	}
	if hc.TimeoutMs > 0 {
		out.TimeoutSec = int(hc.TimeoutMs / 1000)
	}
	if hc.StartPeriodMs > 0 {
		out.StartPeriodSec = int(hc.StartPeriodMs / 1000)
	}
	if hc.Retries > 0 {
		out.UnhealthyThreshold = hc.Retries
	}
	return out
}

// Exists reports whether the instance is still registered in the Manager.
func (a appInstantiator) Exists(instanceID string) bool {
	_, err := a.s.cfg.Manager.Get(instanceID)
	return err == nil
}

// Probe runs the app's health check against the instance's guest. exec runs
// the command in the guest over vsock (exit 0 = healthy) — no network needed.
// http and tcp dial the guest IP directly (reachable from the daemon's root
// netns via the per-sandbox veth, the same path the port-publish forwarder
// uses).
func (a appInstantiator) Probe(ctx context.Context, instanceID string, hc api.HealthCheck) app.Health {
	timeout := time.Duration(hc.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	// exec: run the probe command in the guest; exit 0 = passing, non-zero =
	// failing, a transport error (agent unreachable) = unknown (not a health
	// signal). Runs over vsock, so it works even for a no-network app.
	if hc.Type == "exec" {
		if len(hc.Cmd) == 0 {
			return app.HealthUnknown
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := a.s.cfg.Manager.Exec(pctx, instanceID, wire.ExecRequest{Cmd: hc.Cmd}, io.Discard, io.Discard)
		if err != nil {
			return app.HealthUnknown
		}
		if res.ExitCode == 0 {
			return app.HealthPassing
		}
		return app.HealthFailing
	}

	sb, err := a.s.cfg.Manager.Get(instanceID)
	if err != nil || sb.Network == nil || sb.Network.GuestIP == "" {
		return app.HealthUnknown
	}
	addr := net.JoinHostPort(sb.Network.GuestIP, strconv.Itoa(hc.Port))

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

// Sleep snapshots the instance and stops its VMM (scale-to-zero), keeping the
// record + network so Wake can restore it in place. Returns the durable
// snapshot id captured.
func (a appInstantiator) Sleep(ctx context.Context, instanceID string) (string, error) {
	return a.s.cfg.Manager.SleepInPlace(ctx, instanceID)
}

// Wake restores a slept instance in place, reseeding its CRNG and stepping its
// clock via the guest agent.
func (a appInstantiator) Wake(ctx context.Context, instanceID string) error {
	return a.s.cfg.Manager.WakeInPlace(ctx, instanceID)
}

// SnapshotExists reports whether the durable snapshot still exists (re-adopted
// after a restart if its files survived).
func (a appInstantiator) SnapshotExists(snapshotID string) bool {
	_, err := a.s.cfg.Manager.GetSnapshot(snapshotID)
	return err == nil
}

// WakeFromSnapshot restores the durable sleep snapshot into a fresh instance
// (post-restart wake), returning the new sandbox id. Publish mappings come from
// the app spec, mirroring create.
func (a appInstantiator) WakeFromSnapshot(ctx context.Context, snapshotID string, spec api.AppSpec) (string, error) {
	publish, err := validatePublish(spec.Publish)
	if err != nil {
		return "", err
	}
	sb, err := a.s.cfg.Manager.WakeFromSnapshot(ctx, snapshotID, publish)
	if err != nil {
		return "", err
	}
	return sb.ID, nil
}

// NewAppInstantiator returns the daemon's app.Instantiator. Exposed so the
// process wiring (cmd/crucible) can construct the app manager.
func (s *Server) NewAppInstantiator() app.Instantiator { return appInstantiator{s: s} }
