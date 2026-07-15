package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestAppStateMetrics(t *testing.T) {
	m := New()
	m.SetAppStateSource(func() []AppState {
		return []AppState{
			{Name: "web", Phase: "running", Replicas: 3, ReadyReplicas: 2, SleepCount: 1, LastWakeLatencyMs: 120},
			{Name: "db", Phase: "asleep", Replicas: 1, ReadyReplicas: 0, SleepCount: 4},
		}
	})
	_, body := scrape(t, m)
	for _, want := range []string{
		`app_replicas{app="web"} 3`,
		`app_ready_replicas{app="web"} 2`,
		`app_asleep{app="web"} 0`,
		`app_up{app="web"} 1`,
		`app_sleep_total{app="web"} 1`,
		`app_last_wake_latency_ms{app="web"} 120`,
		`app_asleep{app="db"} 1`,
		`app_up{app="db"} 0`,
		`app_sleep_total{app="db"} 4`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}
	// db is asleep and never woke → no wake-latency series for it.
	if strings.Contains(body, `app_last_wake_latency_ms{app="db"}`) {
		t.Error("db should have no wake-latency series (LastWakeLatencyMs=0)")
	}
}

func TestAppUsageMetrics(t *testing.T) {
	m := New()
	m.SetUsageSource(func() []AppUsageStat {
		return []AppUsageStat{{
			Name:               "web",
			ComputeVCPUSeconds: 7200,
			MemoryMiBSeconds:   3600,
			StorageGiBSeconds:  60,
			Requests:           4,
			RequestsByCode:     map[string]uint64{"2xx": 3, "4xx": 1},
			EgressBytes:        1048576,
		}}
	})
	_, body := scrape(t, m)
	for _, want := range []string{
		`app_usage_compute_vcpu_seconds_total{app="web"} 7200`,
		`app_usage_memory_mib_seconds_total{app="web"} 3600`,
		`app_usage_storage_gib_seconds_total{app="web"} 60`,
		`app_usage_egress_bytes_total{app="web"} 1.048576e+06`,
		`app_usage_requests_total{app="web",code="2xx"} 3`,
		`app_usage_requests_total{app="web",code="4xx"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}
}

func TestAppRequestMetricsAndSync(t *testing.T) {
	m := New()
	m.ObserveAppRequest("web", "2xx", 10*time.Millisecond)
	m.ObserveAppRequest("web", "5xx", 5*time.Millisecond)
	m.ObserveAppRequest("api", "2xx", 3*time.Millisecond)
	m.ObserveAppRequest("", "2xx", time.Millisecond) // empty app ignored

	_, body := scrape(t, m)
	for _, want := range []string{
		`app_requests_total{app="web",code="2xx"} 1`,
		`app_requests_total{app="web",code="5xx"} 1`,
		`app_requests_total{app="api",code="2xx"} 1`,
		`app_request_duration_seconds_count{app="web"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}
	if strings.Contains(body, `app=""`) {
		t.Error("empty app name should not have been recorded")
	}

	// GC: keep only api → web's series are dropped.
	m.SyncApps(map[string]struct{}{"api": {}})
	_, body2 := scrape(t, m)
	if strings.Contains(body2, `app="web"`) {
		t.Errorf("web series survived SyncApps\n---\n%s", body2)
	}
	if !strings.Contains(body2, `app_requests_total{app="api",code="2xx"} 1`) {
		t.Error("api series wrongly removed by SyncApps")
	}
}

func TestAppMetricsNilSafe(t *testing.T) {
	var m *Metrics
	m.SetAppStateSource(func() []AppState { return nil })
	m.ObserveAppRequest("web", "2xx", time.Second)
	m.SyncApps(nil)
	m.SetAppStateSource(nil) // also on a real Metrics
	New().SetAppStateSource(nil)
}
