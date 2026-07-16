// Package metrics holds crucible's Prometheus instrumentation: a small
// fixed set of operational metrics and the /metrics HTTP handler.
//
// The zero use is a nil *Metrics — every method is nil-safe, so callers
// (the sandbox Manager, the daemon) can instrument unconditionally and
// leave metrics unwired in tests and library use. Series are label-free
// in v0.1: crucible is single-operator, so there's no tenant dimension
// to key on and no cardinality to manage.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns crucible's collectors and their registry. Build with New;
// expose with Handler; drive with the Inc/Observe methods.
type Metrics struct {
	reg *prometheus.Registry

	sandboxesCreated        prometheus.Counter
	forkDuration            prometheus.Histogram
	snapshotRestoreDuration prometheus.Histogram
	wakeLatency             prometheus.Histogram
	internalRequests        prometheus.Counter

	// Per-app request metrics (v0.5.4). Push-model with an `app` (and
	// `code` status-class) label; cardinality is bounded to real apps by the
	// proxy (unknown Host headers are never counted) and GC'd via SyncApps.
	appRequests        *prometheus.CounterVec
	appRequestDuration *prometheus.HistogramVec
	appMu              sync.Mutex
	appSeen            map[string]struct{}
}

// New constructs a Metrics with its own registry (not the global default
// one) so tests are isolated and there's no hidden global state.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		sandboxesCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sandboxes_created_total",
			Help: "Total sandboxes created, including cold boots and forks.",
		}),
		forkDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "fork_duration_seconds",
			Help:    "Wall-clock time to bring up one forked sandbox end to end.",
			Buckets: prometheus.DefBuckets,
		}),
		snapshotRestoreDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "snapshot_restore_duration_seconds",
			Help:    "Wall-clock time for the runner to restore a VM from a snapshot.",
			Buckets: prometheus.DefBuckets,
		}),
		wakeLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "app_wake_latency_seconds",
			Help:    "Proxy-observed time to wake a slept app on request (trigger → routable again).",
			Buckets: prometheus.DefBuckets,
		}),
		internalRequests: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "app_internal_requests_total",
			Help: "Total authorized app→app (<app>.internal) requests routed by the ingress proxy.",
		}),
		appRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "app_requests_total",
			Help: "Requests the ingress proxy routed to an app, by HTTP status class.",
		}, []string{"app", "code"}),
		appRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "app_request_duration_seconds",
			Help:    "Ingress-proxy request latency per app (accept → response written).",
			Buckets: prometheus.DefBuckets,
		}, []string{"app"}),
		appSeen: map[string]struct{}{},
	}
	reg.MustRegister(m.sandboxesCreated, m.forkDuration, m.snapshotRestoreDuration,
		m.wakeLatency, m.internalRequests, m.appRequests, m.appRequestDuration)
	return m
}

// SetActiveSandboxSource registers the sandboxes_active gauge, read from
// fn at scrape time. Using a pull-model gauge means there's no
// create/delete bookkeeping that can drift out of sync with reality.
// Call at most once, after the source is available.
func (m *Metrics) SetActiveSandboxSource(fn func() int) {
	if m == nil || fn == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "sandboxes_active",
		Help: "Sandboxes currently live.",
	}, func() float64 { return float64(fn()) }))
}

// SetSnapshotSource registers the snapshots_active gauge, read from fn at scrape
// time (fork snapshots + durable per-instance sleep snapshots). Pull-model, like
// SetActiveSandboxSource. Call at most once.
func (m *Metrics) SetSnapshotSource(fn func() int) {
	if m == nil || fn == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "snapshots_active",
		Help: "Snapshots currently registered (forks + durable sleep snapshots).",
	}, func() float64 { return float64(fn()) }))
}

// SetCertExpirySource registers cert_expiry_seconds: seconds until the
// soonest-expiring managed TLS cert (the alerting signal — "a cert is about to
// expire"). fn returns that value; a large sentinel means "no certs yet / all
// far off", so a `< 7d` alert doesn't fire falsely. Pull-model. Call at most once.
func (m *Metrics) SetCertExpirySource(fn func() float64) {
	if m == nil || fn == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cert_expiry_seconds",
		Help: "Seconds until the soonest-expiring managed TLS certificate.",
	}, fn))
}

// SetDiskSources registers the disk-usage gauges — snapshot_disk_bytes,
// volume_disk_bytes, backup_disk_bytes — each read from its source at scrape
// time, like SetSnapshotSource. All three are sparse-aware (allocated blocks,
// not logical size): scale-to-zero density must not silently become disk
// bloat, and these are what a capacity watermark reads. A nil source skips its
// gauge (no volume manager → no volume/backup series). Call at most once.
func (m *Metrics) SetDiskSources(snapshots, volumes, backups func() int64) {
	if m == nil {
		return
	}
	register := func(name, help string, fn func() int64) {
		if fn == nil {
			return
		}
		m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: name,
			Help: help,
		}, func() float64 { return float64(fn()) }))
	}
	register("snapshot_disk_bytes",
		"Allocated on-disk bytes of registered snapshots (state + memory + rootfs; sparse-aware).", snapshots)
	register("volume_disk_bytes",
		"Allocated on-disk bytes of volume backing files (sparse-aware).", volumes)
	register("backup_disk_bytes",
		"Allocated on-disk bytes of volume backups (sparse-aware; reflink-shared blocks counted per file).", backups)
}

// IncInternalRequest bumps app_internal_requests_total. Call once per authorized
// app→app request the ingress proxy routes.
func (m *Metrics) IncInternalRequest() {
	if m == nil {
		return
	}
	m.internalRequests.Inc()
}

// IncSandboxCreated bumps sandboxes_created_total. Call once per sandbox
// that successfully comes up (cold boot or fork).
func (m *Metrics) IncSandboxCreated() {
	if m == nil {
		return
	}
	m.sandboxesCreated.Inc()
}

// ObserveForkDuration records the end-to-end time to bring up one fork.
func (m *Metrics) ObserveForkDuration(d time.Duration) {
	if m == nil {
		return
	}
	m.forkDuration.Observe(d.Seconds())
}

// ObserveWakeLatency records the proxy-observed time to wake a slept app on a
// request (wake trigger → the app is routable again). This is the product's
// headline scale-to-zero number.
func (m *Metrics) ObserveWakeLatency(d time.Duration) {
	if m == nil {
		return
	}
	m.wakeLatency.Observe(d.Seconds())
}

// ObserveSnapshotRestore records the time spent in the runner's restore
// step (the snapshot → running-VM latency, a subset of a fork).
func (m *Metrics) ObserveSnapshotRestore(d time.Duration) {
	if m == nil {
		return
	}
	m.snapshotRestoreDuration.Observe(d.Seconds())
}

// Gatherer exposes the underlying Prometheus registry so an OTLP metric pipeline
// (the OTel Prometheus bridge) can pull the same series and push them over OTLP
// without redefining any metric. Nil-safe: returns nil on a nil *Metrics.
func (m *Metrics) Gatherer() prometheus.Gatherer {
	if m == nil {
		return nil
	}
	return m.reg
}

// Register adds an extra collector to the metrics registry — used to fold in
// dynamically-scraped guest series (see internal/guestscrape). Errors if the
// collector is invalid; a nil *Metrics is a no-op.
func (m *Metrics) Register(c prometheus.Collector) error {
	if m == nil {
		return nil
	}
	return m.reg.Register(c)
}

// Handler returns the /metrics HTTP handler. On a nil *Metrics it returns
// a 404 handler so the route can be registered unconditionally.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	// ContinueOnError so a single malformed guest-scraped family can't 500 the
	// whole endpoint — it's logged and skipped, the daemon's own metrics still serve.
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}
