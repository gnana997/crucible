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

// AppUsageStat is a scrape-time snapshot of one app's persistent usage metrics
// (cumulative counters). Seconds, not the ledger's internal sub-units — the
// caller has already converted. Wire only LIVE apps: a deleted app drops from
// /metrics (its retained record is still readable via GET /usage).
type AppUsageStat struct {
	Name               string
	ComputeVCPUSeconds float64
	MemoryMiBSeconds   float64
	StorageGiBSeconds  float64
	Requests           uint64
	RequestsByCode     map[string]uint64
}

var (
	descUsageCompute  = prometheus.NewDesc("app_usage_compute_vcpu_seconds_total", "Cumulative vCPU-seconds an app has been awake.", []string{"app"}, nil)
	descUsageMemory   = prometheus.NewDesc("app_usage_memory_mib_seconds_total", "Cumulative MiB-seconds of memory an app has been awake for.", []string{"app"}, nil)
	descUsageStorage  = prometheus.NewDesc("app_usage_storage_gib_seconds_total", "Cumulative GiB-seconds of volume storage an app has occupied.", []string{"app"}, nil)
	descUsageRequests = prometheus.NewDesc("app_usage_requests_total", "Cumulative ingress-proxy requests routed to an app, by status class.", []string{"app", "code"}, nil)
)

// usageCollector emits the per-app persistent-usage-metrics counters by reading
// `source` at each scrape (pull-model, same as appStateCollector). Because the
// daemon exposes the Prometheus registry to the OTel Prometheus bridge, these
// series flow over OTLP too with no extra wiring.
type usageCollector struct{ source func() []AppUsageStat }

func (c usageCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descUsageCompute
	ch <- descUsageMemory
	ch <- descUsageStorage
	ch <- descUsageRequests
}

func (c usageCollector) Collect(ch chan<- prometheus.Metric) {
	for _, a := range c.source() {
		counter := func(d *prometheus.Desc, v float64, labels ...string) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
		}
		counter(descUsageCompute, a.ComputeVCPUSeconds, a.Name)
		counter(descUsageMemory, a.MemoryMiBSeconds, a.Name)
		counter(descUsageStorage, a.StorageGiBSeconds, a.Name)
		for code, n := range a.RequestsByCode {
			counter(descUsageRequests, float64(n), a.Name, code)
		}
	}
}

// SetUsageSource registers the persistent-usage-metrics collector, read from fn
// at scrape time (fn wraps the app manager's live usage). Call at most once.
func (m *Metrics) SetUsageSource(fn func() []AppUsageStat) {
	if m == nil || fn == nil {
		return
	}
	m.reg.MustRegister(usageCollector{source: fn})
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
