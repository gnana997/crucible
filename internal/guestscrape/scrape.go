// Package guestscrape scrapes a Prometheus /metrics endpoint a guest exposes (a
// postgres_exporter, redis_exporter, or an app's own metrics) and folds the series
// into the daemon's own /metrics + OTLP, labeled by app and instance. The daemon
// is already inside the guest's reachability boundary (it dials each guest for
// health checks and packet capture), so it is the natural scraper — no per-guest
// Prometheus, no leaked exporter port.
//
// It is DB-agnostic: it neither parses nor understands the metrics, it only
// federates whatever Prometheus text the guest exposes. Two rules come from the
// serverless nature: a slept / non-routable instance is NEVER scraped and never
// woken (direct-dial, awake-only), and every scrape is size/series/timeout-capped
// because the guest is the untrusted side.
package guestscrape

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Target is one app's metrics endpoint, on its current instance.
type Target struct {
	App      string
	Instance string
	Port     int
	Path     string // defaults to /metrics when empty
}

// TargetSource lists the apps that have a metrics endpoint configured, each with
// its current instance. Wired to the app manager in a later milestone.
type TargetSource interface{ Targets() []Target }

// InstanceResolver maps an instance id to its guest IP and whether it is live
// (awake + routable). A non-live instance is skipped — never scraped, never woken.
type InstanceResolver interface {
	Routable(instanceID string) (guestIP string, live bool)
}

// Options bound the scraper. Zero values fall back to the defaults below.
type Options struct {
	Interval  time.Duration
	Timeout   time.Duration
	MaxBody   int64
	MaxSeries int
	Workers   int
	Logger    *slog.Logger
}

func (o *Options) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = 15 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.MaxBody <= 0 {
		o.MaxBody = 1 << 20 // 1 MiB
	}
	if o.MaxSeries <= 0 {
		o.MaxSeries = 2000
	}
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Manager runs the scrape loop and owns the collector that re-exposes the results.
type Manager struct {
	src  TargetSource
	res  InstanceResolver
	opt  Options
	http *http.Client
	coll *collector
}

// New builds a scrape Manager. Register Collector() with the metrics registry,
// then Run the loop.
func New(src TargetSource, res InstanceResolver, opt Options) *Manager {
	opt.withDefaults()
	return &Manager{
		src:  src,
		res:  res,
		opt:  opt,
		http: &http.Client{Timeout: opt.Timeout},
		coll: newCollector(),
	}
}

// Collector exposes the last-scraped series + scrape health as a Prometheus
// collector to register with the daemon's registry.
func (m *Manager) Collector() prometheus.Collector { return m.coll }

// Run scrapes every Interval until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.opt.Interval)
	defer t.Stop()
	m.scrapeOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.scrapeOnce(ctx)
		}
	}
}

// scrapeOnce scrapes every current target concurrently (bounded) and prunes the
// collector of apps that no longer have a target.
func (m *Manager) scrapeOnce(ctx context.Context) {
	targets := m.src.Targets()
	live := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		live[t.App] = struct{}{}
	}
	m.coll.retain(live)

	sem := make(chan struct{}, m.opt.Workers)
	var wg sync.WaitGroup
	for _, t := range targets {
		t := t
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			m.scrapeTarget(ctx, t)
		}()
	}
	wg.Wait()
}

// scrapeTarget resolves the target's awake instance and scrapes it, recording the
// result (or scrape_up=0 for asleep/failed) in the collector.
func (m *Manager) scrapeTarget(ctx context.Context, t Target) {
	guestIP, liveInstance := m.res.Routable(t.Instance)
	if !liveInstance || guestIP == "" {
		m.coll.set(t.App, t.Instance, nil, 0, 0, false) // asleep → scrape_up=0, no series
		return
	}
	path := t.Path
	if path == "" {
		path = "/metrics"
	}
	url := fmt.Sprintf("http://%s:%d%s", guestIP, t.Port, path)
	start := time.Now()
	fams, samples, err := m.fetch(ctx, url)
	dur := time.Since(start).Seconds()
	if err != nil {
		m.coll.set(t.App, t.Instance, nil, 0, dur, false)
		m.opt.Logger.Warn("guest metrics scrape failed", "app", t.App, "instance", t.Instance, "err", err)
		return
	}
	m.coll.set(t.App, t.Instance, fams, samples, dur, true)
}

// fetch GETs the endpoint (capped), parses the exposition, and returns the metric
// families + total series count. Errors if the body exceeds MaxBody, the series
// count exceeds MaxSeries, or the response is not 200 / not parseable.
func (m *Manager) fetch(ctx context.Context, url string) (map[string]*dto.MetricFamily, int, error) {
	ctx, cancel := context.WithTimeout(ctx, m.opt.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("scrape %s: status %d", url, resp.StatusCode)
	}
	// +1 so a body exactly at the cap still reads fully; a larger one trips the guard.
	body, err := io.ReadAll(io.LimitReader(resp.Body, m.opt.MaxBody+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(body)) > m.opt.MaxBody {
		return nil, 0, fmt.Errorf("scrape %s: body exceeds %d bytes", url, m.opt.MaxBody)
	}
	// UTF8 scheme is permissive (accepts legacy exporter names too); the zero-value
	// parser has an invalid scheme and would panic.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("scrape %s: parse: %w", url, err)
	}
	n := 0
	for _, fam := range fams {
		n += len(fam.Metric)
	}
	if n > m.opt.MaxSeries {
		return nil, 0, fmt.Errorf("scrape %s: %d series exceeds cap %d", url, n, m.opt.MaxSeries)
	}
	return fams, n, nil
}
