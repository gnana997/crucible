package api

import (
	"time"

	"github.com/gnana997/crucible/sdk/wire"
)

// AppSpec is the desired state of a durable app: a named, long-lived
// workload the daemon keeps a healthy instance of, surviving daemon
// restarts by re-creating the instance from this spec. Fields mirror the
// OCI image config plus docker-run-style overrides — Fly's fly.toml minus
// the fleet fields.
//
// An app owns at most one running instance (a sandbox) at a time in
// v0.4.0; multi-instance is a later release.
type AppSpec struct {
	// Name is the app's stable, user-facing identity (unique per daemon).
	// It becomes the routing hostname once name-based routing lands, so it
	// is validated as a DNS label: lowercase alphanumeric + hyphens.
	Name string `json:"name"`

	// Image is the OCI image the instance boots from. Required.
	Image *ImageRef `json:"image,omitempty"`

	// Pull mirrors CreateSandboxRequest.Pull ("missing" | "always" | "never").
	Pull string `json:"pull,omitempty"`

	// Sizing for the instance. Zero values take the daemon defaults.
	VCPUs     int   `json:"vcpus,omitempty"`
	MemoryMiB int   `json:"memory_mib,omitempty"`
	DiskBytes int64 `json:"disk_bytes,omitempty"`

	// Env is added to the instance's environment.
	Env map[string]string `json:"env,omitempty"`

	// Publish maps host ports to the instance's guest ports (docker -p).
	Publish []PortMapping `json:"publish,omitempty"`

	// PublishAll publishes every port the image declares with EXPOSE, each
	// to the same host port number (docker -P, but deterministic). tcp
	// only; an explicit Publish entry for a guest port takes precedence.
	PublishAll bool `json:"publish_all,omitempty"`

	// Port is the guest port the ingress proxy forwards to when routing this
	// app by name (v0.4.2). Zero lets the daemon default it from a single
	// published/EXPOSEd port. Independent of Publish, which is the direct
	// host-port bypass path.
	Port int `json:"port,omitempty"`

	// Network is the per-app egress policy; nil means default-deny.
	Network *NetworkRequest `json:"network,omitempty"`

	// Service overrides the image entrypoint (the long-lived process the
	// guest supervisor runs). Nil uses the image's ENTRYPOINT/CMD.
	Service *wire.ServiceSpec `json:"service,omitempty"`

	// Restart is the INSTANCE-level restart policy the daemon reconciler
	// enforces (boot a replacement when the whole instance is gone). It is
	// distinct from wire.ServiceSpec.Restart, which is the guest
	// supervisor restarting a crashed *process* inside a live instance.
	Restart wire.RestartPolicy `json:"restart"`

	// Health is the daemon-side health check; nil means "process alive is
	// healthy". An instance failing health past its threshold is restarted.
	Health *HealthCheck `json:"health,omitempty"`
}

// HealthCheck configures daemon-side probing of an app's instance.
type HealthCheck struct {
	// Type is "http", "tcp", or "exec". Empty disables probing.
	Type string `json:"type"`

	// Path and Port configure the HTTP probe (GET Path on guest:Port,
	// expect 2xx). Port also serves the TCP probe (connect succeeds).
	Path string `json:"path,omitempty"`
	Port int    `json:"port,omitempty"`

	// Cmd is the exec probe: run it in-guest, healthy iff exit 0.
	Cmd []string `json:"cmd,omitempty"`

	// Timing. Zero values take conservative defaults at probe time.
	IntervalSec        int `json:"interval_s,omitempty"`
	TimeoutSec         int `json:"timeout_s,omitempty"`
	HealthyThreshold   int `json:"healthy_threshold,omitempty"`
	UnhealthyThreshold int `json:"unhealthy_threshold,omitempty"`
	// StartPeriodSec is a grace window after boot during which failing
	// probes don't count against the app (slow-starting servers).
	StartPeriodSec int `json:"start_period_s,omitempty"`
}

// AppResponse is an app's desired state (the embedded AppSpec) plus its
// observed status and control metadata.
type AppResponse struct {
	// ID is the generated app id (app_...); Name (in AppSpec) is the
	// user-facing identity.
	ID string `json:"id"`

	AppSpec

	// DesiredState is "running" (the daemon keeps an instance alive) or
	// "stopped" (no instance, spec retained).
	DesiredState string `json:"desired_state"`

	// Generation increments on every spec update; the reconciler uses it
	// to detect a spec change that needs a redeploy.
	Generation uint64 `json:"generation"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Status is the observed state, populated by the reconciler. Nil/zero
	// before the reconciler has acted.
	Status *AppStatus `json:"status,omitempty"`
}

// AppStatus is an app's observed (not persisted) runtime state.
type AppStatus struct {
	// InstanceID is the sandbox currently backing the app, if any.
	InstanceID string `json:"instance_id,omitempty"`

	// InstanceGeneration is the spec generation the current instance was booted
	// from. It lags Generation while a rolling update is in progress or after a
	// failed update (the old instance still serves the previous generation).
	InstanceGeneration uint64 `json:"instance_generation,omitempty"`

	// Phase is one of: pending, running, unhealthy, crashlooping, stopped.
	Phase string `json:"phase"`

	// Health is healthy | unhealthy | unknown (unknown when no probe or
	// still in the start period).
	Health string `json:"health,omitempty"`

	// Restarts counts instance-level restarts the reconciler has done.
	Restarts int `json:"restarts,omitempty"`

	// LastError is the most recent reconcile/boot failure, if any.
	LastError string `json:"last_error,omitempty"`
}

// CreateAppRequest is the body of POST /apps.
type CreateAppRequest struct {
	AppSpec

	// DesiredState defaults to "running" when empty.
	DesiredState string `json:"desired_state,omitempty"`
}

// AppListResponse wraps the app list.
type AppListResponse struct {
	Apps []AppResponse `json:"apps"`
}
