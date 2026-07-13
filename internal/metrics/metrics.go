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

// Handler returns the /metrics HTTP handler. On a nil *Metrics it returns
// a 404 handler so the route can be registered unconditionally.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
