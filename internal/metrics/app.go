package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// AppState is a point-in-time snapshot of one app, read at scrape time to emit
// per-app lifecycle metrics with no push bookkeeping: an app that vanishes from
// the source simply stops being reported, so there is no cardinality to GC.
type AppState struct {
	Name              string
	Phase             string // running | asleep | pending | stopped | …
	Replicas          int
	ReadyReplicas     int
	SleepCount        int
	LastWakeLatencyMs int64
}

var (
	descAppReplicas   = prometheus.NewDesc("app_replicas", "Desired instances for an app.", []string{"app"}, nil)
	descAppReady      = prometheus.NewDesc("app_ready_replicas", "Ready (serving) instances for an app.", []string{"app"}, nil)
	descAppAsleep     = prometheus.NewDesc("app_asleep", "1 if the app is scaled to zero (asleep), else 0.", []string{"app"}, nil)
	descAppUp         = prometheus.NewDesc("app_up", "1 if the app has a running instance, else 0.", []string{"app"}, nil)
	descAppSleepTotal = prometheus.NewDesc("app_sleep_total", "Sleep cycles the app has been through.", []string{"app"}, nil)
	descAppWakeMs     = prometheus.NewDesc("app_last_wake_latency_ms", "Most recent wake latency for the app, in milliseconds.", []string{"app"}, nil)
)

// appStateCollector emits the per-app lifecycle gauges by reading `source` at
// each scrape (pull-model, like sandboxes_active).
type appStateCollector struct{ source func() []AppState }

func (c appStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descAppReplicas
	ch <- descAppReady
	ch <- descAppAsleep
	ch <- descAppUp
	ch <- descAppSleepTotal
	ch <- descAppWakeMs
}

func (c appStateCollector) Collect(ch chan<- prometheus.Metric) {
	for _, a := range c.source() {
		gauge := func(d *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, a.Name)
		}
		gauge(descAppReplicas, float64(a.Replicas))
		gauge(descAppReady, float64(a.ReadyReplicas))
		gauge(descAppAsleep, boolf(a.Phase == "asleep"))
		gauge(descAppUp, boolf(a.Phase == "running"))
		ch <- prometheus.MustNewConstMetric(descAppSleepTotal, prometheus.CounterValue, float64(a.SleepCount), a.Name)
		if a.LastWakeLatencyMs > 0 {
			gauge(descAppWakeMs, float64(a.LastWakeLatencyMs))
		}
	}
}

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// SetAppStateSource registers the per-app lifecycle collector, read from fn at
// scrape time (fn typically wraps app.Manager.List). Pull-model, so there is no
// per-app bookkeeping. Call at most once.
func (m *Metrics) SetAppStateSource(fn func() []AppState) {
	if m == nil || fn == nil {
		return
	}
	m.reg.MustRegister(appStateCollector{source: fn})
}

// ObserveAppRequest records one ingress-proxy request for a KNOWN app: bumps
// app_requests_total{app,code} and observes app_request_duration_seconds{app}.
// The caller (proxy) must pass only real app names and a bounded status class
// (see telemetry.StatusClass) so label cardinality stays fixed.
func (m *Metrics) ObserveAppRequest(app, code string, d time.Duration) {
	if m == nil || app == "" {
		return
	}
	m.appMu.Lock()
	m.appSeen[app] = struct{}{}
	m.appMu.Unlock()
	m.appRequests.WithLabelValues(app, code).Inc()
	m.appRequestDuration.WithLabelValues(app).Observe(d.Seconds())
}

// SyncApps GCs the push-model per-app request series for apps no longer live,
// so a deleted app's counters don't linger for the daemon's lifetime. Call
// periodically with the current app set (never during a scrape).
func (m *Metrics) SyncApps(live map[string]struct{}) {
	if m == nil {
		return
	}
	m.appMu.Lock()
	defer m.appMu.Unlock()
	for app := range m.appSeen {
		if _, ok := live[app]; ok {
			continue
		}
		m.appRequests.DeletePartialMatch(prometheus.Labels{"app": app})
		m.appRequestDuration.DeleteLabelValues(app)
		delete(m.appSeen, app)
	}
}
