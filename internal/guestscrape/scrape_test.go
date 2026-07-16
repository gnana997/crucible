package guestscrape

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type fakeSource struct{ t []Target }

func (f *fakeSource) Targets() []Target { return f.t }

type fakeResolver struct {
	ip   map[string]string
	live map[string]bool
}

func (f *fakeResolver) Routable(id string) (string, bool) { return f.ip[id], f.live[id] }

const exposition = `# HELP pg_up Postgres up
# TYPE pg_up gauge
pg_up 1
# HELP pg_conn Connections by database
# TYPE pg_conn gauge
pg_conn{datname="app"} 7
`

// value looks up a gathered metric by name + a subset of labels.
func value(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.Metric {
			if labelsMatch(m, labels) {
				return metricVal(m), true
			}
		}
	}
	return 0, false
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.Label {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func metricVal(m *dto.Metric) float64 {
	switch {
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	case m.Counter != nil:
		return m.Counter.GetValue()
	case m.Untyped != nil:
		return m.Untyped.GetValue()
	}
	return 0
}

func setup(t *testing.T, opt Options) (*Manager, *prometheus.Registry, *fakeResolver, *fakeSource) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(exposition))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	src := &fakeSource{t: []Target{{App: "db", Instance: "i1", Port: port, Path: "/metrics"}}}
	res := &fakeResolver{ip: map[string]string{"i1": "127.0.0.1"}, live: map[string]bool{"i1": true}}
	m := New(src, res, opt)
	reg := prometheus.NewRegistry()
	if err := reg.Register(m.Collector()); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, reg, res, src
}

func TestScrapeMergesGuestSeriesWithLabels(t *testing.T) {
	m, reg, _, _ := setup(t, Options{})
	m.scrapeOnce(context.Background())

	// The scraped series appear with app+instance labels preserved alongside the
	// exporter's own labels.
	if v, ok := value(t, reg, "pg_conn", map[string]string{"app": "db", "instance": "i1", "datname": "app"}); !ok || v != 7 {
		t.Fatalf("pg_conn = %v (ok=%v), want 7", v, ok)
	}
	// Scrape health: up=1, 2 series ingested (pg_up + pg_conn).
	if v, ok := value(t, reg, "crucible_guest_scrape_up", map[string]string{"app": "db", "instance": "i1"}); !ok || v != 1 {
		t.Fatalf("scrape_up = %v (ok=%v), want 1", v, ok)
	}
	if v, _ := value(t, reg, "crucible_guest_scrape_samples", map[string]string{"app": "db"}); v != 2 {
		t.Fatalf("scrape_samples = %v, want 2", v)
	}
}

func TestScrapeSkipsAsleepInstanceAndNeverWakes(t *testing.T) {
	m, reg, res, _ := setup(t, Options{})
	// Instance not routable (asleep). It must be skipped — up=0, no series, and the
	// stub is never hit (a hit would mean we tried to reach a slept guest).
	res.live["i1"] = false
	m.scrapeOnce(context.Background())

	if v, ok := value(t, reg, "crucible_guest_scrape_up", map[string]string{"app": "db", "instance": "i1"}); !ok || v != 0 {
		t.Fatalf("scrape_up for asleep = %v (ok=%v), want 0", v, ok)
	}
	if _, ok := value(t, reg, "pg_conn", map[string]string{"app": "db"}); ok {
		t.Fatal("a slept instance must expose no scraped series")
	}
}

func TestScrapeSeriesCapDropsScrape(t *testing.T) {
	m, reg, _, _ := setup(t, Options{MaxSeries: 1}) // exposition has 2 series
	m.scrapeOnce(context.Background())
	if v, ok := value(t, reg, "crucible_guest_scrape_up", map[string]string{"app": "db"}); !ok || v != 0 {
		t.Fatalf("scrape_up over series cap = %v, want 0", v)
	}
	if _, ok := value(t, reg, "pg_conn", nil); ok {
		t.Fatal("no series should be ingested when the cap is exceeded")
	}
}

func TestScrapeBodyCapDropsScrape(t *testing.T) {
	m, reg, _, _ := setup(t, Options{MaxBody: 8}) // exposition is far larger than 8 bytes
	m.scrapeOnce(context.Background())
	if v, ok := value(t, reg, "crucible_guest_scrape_up", map[string]string{"app": "db"}); !ok || v != 0 {
		t.Fatalf("scrape_up over body cap = %v, want 0", v)
	}
}

func TestScrapeRetainsOnlyCurrentApps(t *testing.T) {
	m, reg, _, src := setup(t, Options{})
	m.scrapeOnce(context.Background())
	if _, ok := value(t, reg, "crucible_guest_scrape_up", map[string]string{"app": "db"}); !ok {
		t.Fatal("db should be present after scraping")
	}
	// App removed from the target set → pruned on the next pass.
	src.t = nil
	m.scrapeOnce(context.Background())
	if _, ok := value(t, reg, "crucible_guest_scrape_up", map[string]string{"app": "db"}); ok {
		t.Fatal("a removed app must be pruned from the collector")
	}
}
