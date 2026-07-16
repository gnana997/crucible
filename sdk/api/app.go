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
// An app owns one or more running instances (sandboxes): a single instance by
// default, or several when horizontally scaled out (min_scale/max_scale, v0.5.2).
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

	// Volumes are persistent block-device volumes mounted into the instance.
	// A volume app is single-writer: it redeploys via destroy-then-boot and
	// sleeps via stop/start (never the snapshot flip), and its data survives
	// redeploys, sleep, and daemon restarts.
	Volumes []VolumeMount `json:"volumes,omitempty"`

	// Env is added to the instance's environment.
	Env map[string]string `json:"env,omitempty"`

	// Publish maps host ports to the instance's guest ports (docker -p).
	Publish []PortMapping `json:"publish,omitempty"`

	// PublishAll publishes every port the image declares with EXPOSE, each
	// to the same host port number (docker -P, but deterministic). tcp
	// only; an explicit Publish entry for a guest port takes precedence.
	PublishAll bool `json:"publish_all,omitempty"`

	// MetricsPort, when set, is a guest port exposing a Prometheus /metrics
	// endpoint (a postgres_exporter, redis_exporter, or the app's own metrics)
	// that the daemon scrapes and folds into its own /metrics + OTLP, labeled by
	// app + instance. Scraped only while the instance is awake (a slept app is
	// never scraped or woken). Not published — the daemon reaches it host-side.
	// 0 = no scrape.
	MetricsPort int `json:"metrics_port,omitempty"`

	// MetricsPath is the scrape path on MetricsPort (default /metrics).
	MetricsPath string `json:"metrics_path,omitempty"`

	// Port is the guest port the ingress proxy forwards to when routing this
	// app by name (v0.4.2). Zero lets the daemon default it from a single
	// published/EXPOSEd port. Independent of Publish, which is the direct
	// host-port bypass path.
	Port int `json:"port,omitempty"`

	// Domains are custom domains (FQDNs) attached to this app: a request whose
	// Host is one of them routes here, and — in terminate mode — the proxy
	// obtains a cert for it. Managed by `app domain add/rm`, NOT by `app update`
	// (which preserves them), and globally unique (one domain → one app).
	Domains []string `json:"domains,omitempty"`

	// HTTPRedirect controls whether the proxy 301-redirects plaintext :80
	// requests for this app to HTTPS. Nil/true (default) redirects; false serves
	// plain HTTP (for apps that legitimately need it). Ignored in passthrough
	// mode (the guest owns :443) and when TLS termination is disabled.
	HTTPRedirect *bool `json:"http_redirect,omitempty"`

	// TLSMode selects how the ingress proxy handles this app's HTTPS on :443.
	// "" / "terminate" (default): the proxy terminates TLS with a managed cert
	// and forwards plaintext to the guest. "passthrough": the proxy pipes the
	// raw TLS stream to the guest, which owns its own certificate (for apps that
	// terminate their own TLS or speak a non-HTTP TLS protocol). Only meaningful
	// when the daemon has a cert source configured; otherwise :443 is always
	// passthrough.
	TLSMode string `json:"tls_mode,omitempty"`

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

	// Sleep configures scale-to-zero. Nil disables it (today's always-on
	// behavior). When set, an idle instance is snapshotted and its VMM
	// stopped — freeing RAM while keeping the app instantly addressable —
	// then restored in place on the next request (wake).
	Sleep *SleepPolicy `json:"sleep,omitempty"`

	// CanCall lists the apps this app may reach over the internal zone
	// (<app>.internal) through the ingress proxy — app→app service networking
	// (v0.5.1). Default-deny: empty means this app may call no other app. Requires
	// the daemon's --internal-networking. Each entry is an app name (DNS label).
	CanCall []string `json:"can_call,omitempty"`

	// InternalPorts are the TCP ports this app EXPOSES to authorized peers on the
	// internal zone (a peer reaches them at <app>.internal:Port), each with a
	// protocol handler (see InternalPort). Empty means the app exposes nothing
	// app→app at L4. Requires the daemon's --internal-l4; a caller still needs
	// --can-call to this app. Where CanCall is the outbound grant, InternalPorts is
	// the inbound surface. (v0.9.5)
	InternalPorts []InternalPort `json:"internal_ports,omitempty"`

	// SecretEnvFrom names encrypted secret bundles whose every key is injected as
	// an environment variable at boot (envFrom). The bundles' values live only in
	// the daemon's encrypted secret store — never here — so this carries only the
	// bundle names, safe in `app get` and backups.
	SecretEnvFrom []string `json:"secret_env_from,omitempty"`
}

// InternalPort is one TCP port an app exposes to authorized peers on the internal
// zone (v0.9.5 app→app L4). Proto selects how the ingress proxy handles it:
//
//   - "tcp" (default): a blind byte splice — any protocol, and TLS passes through
//     untouched, so a client speaks the service's native wire protocol end to end
//     (what a database endpoint needs).
//   - "http": the connection is routed per-request through the L7 proxy, keeping
//     load-balancing across replicas and status-class metrics.
//
// The port is reached at <app>.internal:Port by a peer granted --can-call.
type InternalPort struct {
	Port  int    `json:"port"`
	Proto string `json:"proto,omitempty"` // "tcp" (default) | "http"
}

// SleepPolicy configures an app's scale-to-zero behavior.
type SleepPolicy struct {
	// IdleTimeoutSec sleeps the instance after this many seconds with no
	// activity. Zero disables automatic idle-sleep; manual sleep/wake still
	// works. (Idle detection consumes this; the manual path does not.)
	IdleTimeoutSec int `json:"idle_timeout_s,omitempty"`

	// MinScale is the minimum number of warm instances. 0 enables
	// scale-to-zero — the instance may sleep to ~zero RAM. >=1 keeps at
	// least one instance always running (sleep disabled in practice).
	MinScale int `json:"min_scale"`

	// MaxScale is the ceiling for horizontal autoscaling (v0.5.2). When it
	// exceeds the running floor (max(MinScale,1)), the daemon autoscales the app
	// between that floor and MaxScale on request concurrency. 0 (or <= the floor)
	// disables autoscaling — the app stays at MinScale.
	MaxScale int `json:"max_scale,omitempty"`

	// TargetConcurrency is the desired in-flight requests per instance the
	// autoscaler aims for (replicas ≈ ceil(observed concurrency / target)). Zero
	// takes a conservative default.
	TargetConcurrency int `json:"target_concurrency,omitempty"`

	// ConnIdleTimeoutSec is how long a TCP connection through the wake-on-TCP
	// forwarder may sit idle (no bytes either way) before the forwarder closes
	// it — so a scale-to-zero app whose clients hold pooled connections can still
	// reach zero connections and sleep. 0 defaults to IdleTimeoutSec. Ignored when
	// KeepConnections is set. (Only meaningful for a scale-to-zero published app.)
	ConnIdleTimeoutSec int `json:"conn_idle_timeout_s,omitempty"`

	// KeepConnections disables idle-connection reaping: the forwarder never closes
	// a connection on silence (only TCP keepalive reaps a genuinely dead peer), so
	// the app sleeps only when the last client disconnects. This is the mode for
	// connection-scoped workloads — pub/sub, LISTEN/NOTIFY, streaming — where an
	// idle-but-live connection must not be dropped.
	KeepConnections bool `json:"keep_connections,omitempty"`
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

	// InternalVIP is the app's stable per-app virtual IP on the internal zone,
	// assigned by the daemon when --internal-l4 is on and the app declares
	// InternalPorts. Peers reach it as <app>.internal (which resolves to this
	// address). Empty when L4 app→app is off or the app exposes no internal ports.
	// (v0.9.5)
	InternalVIP string `json:"internal_vip,omitempty"`

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

	// Phase is one of: pending, running, unhealthy, crashlooping, stopped,
	// asleep (snapshotted, VMM stopped, ~0 RAM), waking (restore in progress).
	Phase string `json:"phase"`

	// Health is healthy | unhealthy | unknown (unknown when no probe or
	// still in the start period).
	Health string `json:"health,omitempty"`

	// Restarts counts instance-level restarts the reconciler has done.
	Restarts int `json:"restarts,omitempty"`

	// LastError is the most recent reconcile/boot failure, if any.
	LastError string `json:"last_error,omitempty"`

	// LastWakeLatencyMs is the most recent wake duration in milliseconds
	// (from the wake trigger to the instance serving again). Zero until the
	// app has woken at least once.
	LastWakeLatencyMs int64 `json:"last_wake_latency_ms,omitempty"`

	// SleepCount is how many sleep cycles this app has been through since the
	// daemon started.
	SleepCount int `json:"sleep_count,omitempty"`

	// Replicas is the desired number of instances (1 for a single-instance app;
	// higher for a horizontally-scaled app, v0.5.2). ReadyReplicas is how many are
	// currently up. Instances lists them — Instances[0] is the primary, whose id
	// mirrors InstanceID above. Single-instance clients can keep reading the
	// scalar fields; these describe the whole endpoint set.
	Replicas      int              `json:"replicas,omitempty"`
	ReadyReplicas int              `json:"ready_replicas,omitempty"`
	Instances     []InstanceStatus `json:"instances,omitempty"`
}

// InstanceStatus is one instance in an app's endpoint set.
type InstanceStatus struct {
	InstanceID string `json:"instance_id"`
	Generation uint64 `json:"generation,omitempty"`
	Health     string `json:"health,omitempty"` // healthy | unhealthy | unknown
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

// AddDomainRequest attaches a custom domain to an app (POST /apps/{name}/domains).
type AddDomainRequest struct {
	Domain string `json:"domain"`
}

// DomainListResponse is an app's attached custom domains (GET .../domains).
// Domains is always the plain name list (unchanged, back-compatible); Details is
// populated only when the request asks for it (?detail=1) and carries per-domain
// TLS/certificate status.
type DomainListResponse struct {
	Domains []string       `json:"domains"`
	Details []DomainDetail `json:"details,omitempty"`
}

// DomainDetail is one domain's routing + certificate status for an app.
type DomainDetail struct {
	// Domain is the FQDN. Generated is true for the app's built-in
	// <app>.<proxy-domain> name (vs a custom domain the user attached).
	Domain    string `json:"domain"`
	Generated bool   `json:"generated,omitempty"`
	// TLSMode is how the ingress proxy handles :443 for this app: "terminate"
	// (the proxy manages the cert) or "passthrough" (the guest owns it).
	TLSMode string `json:"tls_mode"`
	// Cert is the certificate status (see CertStatus). For a passthrough app it
	// is always {state: passthrough} — the daemon manages no cert.
	Cert CertStatus `json:"cert"`
}

// CertStatus is the state of the TLS certificate the daemon manages for a
// domain. States:
//   - passthrough: the app is passthrough-mode; the guest owns its cert.
//   - pending:     terminate-mode, no cert obtained yet (issuance in flight).
//   - active:      a valid managed (or ACME) cert is served.
//   - expiring:    active, but within the renewal lead — renewal is due/underway.
//   - failed:      the last ACME attempt errored (LastError/LastAttempt set) —
//     commonly the domain's DNS isn't pointed at the host.
//   - manual:      served from an operator-supplied drop-in cert (never renewed).
type CertStatus struct {
	State       string     `json:"state"`
	NotAfter    *time.Time `json:"not_after,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastAttempt *time.Time `json:"last_attempt,omitempty"`
}

// AppUsage is an app's persistent usage metrics: durable, cumulative,
// monotonic counters the daemon keeps across restarts. Values are cumulative —
// a reader takes deltas between two reads (and converts seconds→hours) itself;
// the daemon does no rating or aggregation. Time-based dimensions accrue only
// while a dimension is "live": compute/memory while the app is awake (a slept
// app burns none), storage for as long as the volume exists (awake or asleep).
type AppUsage struct {
	AppID   string `json:"app_id"`
	AppName string `json:"app_name"`

	// ComputeVCPUSeconds is Σ vCPUs × seconds awake; MemoryMiBSeconds is
	// Σ MemoryMiB × seconds awake; StorageGiBSeconds is Σ volume-GiB × seconds.
	ComputeVCPUSeconds float64 `json:"compute_vcpu_seconds"`
	MemoryMiBSeconds   float64 `json:"memory_mib_seconds"`
	StorageGiBSeconds  float64 `json:"storage_gib_seconds"`

	// Requests is total ingress-proxy requests routed to the app;
	// RequestsByCode splits it by HTTP status class ("2xx", "4xx", …).
	Requests       uint64            `json:"requests"`
	RequestsByCode map[string]uint64 `json:"requests_by_code,omitempty"`

	// EgressBytes is cumulative external egress (bytes the app sent out to the
	// network), summed across its instances.
	EgressBytes uint64 `json:"egress_bytes"`

	// UpdatedAt is when the counters were last persisted. FinalizedAt, when set,
	// means the app was deleted and this is its retained final usage.
	UpdatedAt   time.Time  `json:"updated_at"`
	FinalizedAt *time.Time `json:"finalized_at,omitempty"`
}

// UsageListResponse is every app's usage (GET /usage), including retained
// records for deleted apps. SnapshotUnixNano is when the reading was taken, so
// a reader can reconcile cumulative values across reads.
type UsageListResponse struct {
	Usage            []AppUsage `json:"usage"`
	SnapshotUnixNano int64      `json:"snapshot_unix_nano"`
}

// AppEvent is one app lifecycle transition (GET /events). Seq is a monotonic
// per-daemon cursor a reader resumes from.
type AppEvent struct {
	Seq      uint64    `json:"seq"`
	Time     time.Time `json:"time"`
	App      string    `json:"app"`
	AppID    string    `json:"app_id"`
	Instance string    `json:"instance,omitempty"`
	// Type is one of: created, updated, deleted, domain_added, domain_removed,
	// phase_changed (Attrs carry from/to and, on wake, wake_latency_ms),
	// health_changed, rollout.
	Type   string         `json:"type"`
	Reason string         `json:"reason,omitempty"`
	Attrs  map[string]any `json:"attrs,omitempty"`
}

// EventsResponse is a batch of lifecycle events (GET /events?since=<seq>) plus
// the current max cursor, so a reader resumes even when the batch is empty. The
// event stream is an in-memory ring: a reader offline longer than the ring loses
// old events, but usage totals stay correct (reconcile against GET /usage).
type EventsResponse struct {
	Events []AppEvent `json:"events"`
	Cursor uint64     `json:"cursor"`
}

// SecretRequest stores or replaces a secret bundle (PUT /secrets/{name}): a set
// of key→value pairs, sealed encrypted at rest. When Merge is true the keys are
// merged into an existing bundle (values are updated, others kept); otherwise the
// bundle is replaced with exactly Data.
type SecretRequest struct {
	Data  map[string]string `json:"data"`
	Merge bool              `json:"merge,omitempty"`
}

// SecretListResponse is the names of the stored secret bundles (GET /secrets) —
// never their contents.
type SecretListResponse struct {
	Secrets []string `json:"secrets"`
}

// SecretKeysResponse is one bundle's key NAMES (GET /secrets/{name}) — never the
// values.
type SecretKeysResponse struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}
