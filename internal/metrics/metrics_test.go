package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Metrics) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestExposition(t *testing.T) {
	m := New()
	m.SetActiveSandboxSource(func() int { return 3 })
	m.IncSandboxCreated()
	m.IncSandboxCreated()
	m.ObserveForkDuration(250 * time.Millisecond)
	m.ObserveSnapshotRestore(120 * time.Millisecond)

	code, body := scrape(t, m)
	if code != 200 {
		t.Fatalf("scrape status = %d, want 200", code)
	}
	for _, want := range []string{
		"sandboxes_created_total 2",
		"sandboxes_active 3",
		"fork_duration_seconds_count 1",
		"snapshot_restore_duration_seconds_count 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestActiveGaugeReadsAtScrapeTime(t *testing.T) {
	n := 0
	m := New()
	m.SetActiveSandboxSource(func() int { return n })

	n = 5
	if _, body := scrape(t, m); !strings.Contains(body, "sandboxes_active 5") {
		t.Errorf("want sandboxes_active 5, body:\n%s", body)
	}
	n = 2
	if _, body := scrape(t, m); !strings.Contains(body, "sandboxes_active 2") {
		t.Errorf("gauge did not re-read; want sandboxes_active 2, body:\n%s", body)
	}
}

func TestNilMetricsIsNoop(t *testing.T) {
	var m *Metrics // nil receiver

	// None of these should panic.
	m.IncSandboxCreated()
	m.ObserveForkDuration(time.Second)
	m.ObserveSnapshotRestore(time.Second)
	m.SetActiveSandboxSource(func() int { return 1 })

	if code, _ := scrape(t, m); code != 404 {
		t.Fatalf("nil Handler status = %d, want 404", code)
	}
}
