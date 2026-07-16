package guestscrape

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Meta-metric descriptors (scrape health), Prometheus convention.
var (
	upDesc = prometheus.NewDesc("crucible_guest_scrape_up",
		"1 if the last guest metrics scrape for this app+instance succeeded, else 0.",
		[]string{"app", "instance"}, nil)
	durDesc = prometheus.NewDesc("crucible_guest_scrape_duration_seconds",
		"Duration of the last guest metrics scrape.",
		[]string{"app", "instance"}, nil)
	samplesDesc = prometheus.NewDesc("crucible_guest_scrape_samples",
		"Number of series ingested by the last guest metrics scrape.",
		[]string{"app", "instance"}, nil)
)

// entry is the last-scraped state for one app.
type entry struct {
	instance string
	up       bool
	dur      float64 // seconds
	samples  int
	families map[string]*dto.MetricFamily
}

// collector re-exposes the last-scraped guest series + scrape-health meta-metrics.
// Unchecked (Describe emits nothing) so metric names/labels can be fully dynamic.
type collector struct {
	mu      sync.Mutex
	byApp   map[string]entry
	helpFor map[string]string // first-seen help per metric name, for consistency
}

func newCollector() *collector {
	return &collector{byApp: map[string]entry{}, helpFor: map[string]string{}}
}

// set records a scrape result: families+samples on success (up=true), or nil with
// up=false for an asleep/failed instance.
func (c *collector) set(app, instance string, families map[string]*dto.MetricFamily, samples int, durSeconds float64, up bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byApp[app] = entry{instance: instance, up: up, dur: durSeconds, samples: samples, families: families}
}

// retain drops entries for apps no longer in the target set (e.g. deleted apps).
func (c *collector) retain(keep map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for app := range c.byApp {
		if _, ok := keep[app]; !ok {
			delete(c.byApp, app)
		}
	}
}

func (c *collector) Describe(chan<- *prometheus.Desc) {} // unchecked

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for app, e := range c.byApp {
		up := 0.0
		if e.up {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, up, app, e.instance)
		ch <- prometheus.MustNewConstMetric(durDesc, prometheus.GaugeValue, e.dur, app, e.instance)
		ch <- prometheus.MustNewConstMetric(samplesDesc, prometheus.GaugeValue, float64(e.samples), app, e.instance)
		if !e.up {
			continue
		}
		for _, fam := range e.families {
			c.emitFamily(ch, fam, app, e.instance)
		}
	}
}

// emitFamily reconstructs a scraped family as const metrics, prepending app +
// instance labels. Help is normalized to the first-seen value per metric name so
// two instances of the same exporter don't trip promhttp's consistency check.
func (c *collector) emitFamily(ch chan<- prometheus.Metric, fam *dto.MetricFamily, app, instance string) {
	name := fam.GetName()
	if name == "" {
		return
	}
	help, ok := c.helpFor[name]
	if !ok {
		help = fam.GetHelp()
		c.helpFor[name] = help
	}
	for _, m := range fam.Metric {
		labelNames := make([]string, 0, len(m.Label)+2)
		labelVals := make([]string, 0, len(m.Label)+2)
		labelNames = append(labelNames, "app", "instance")
		labelVals = append(labelVals, app, instance)
		for _, lp := range m.Label {
			labelNames = append(labelNames, lp.GetName())
			labelVals = append(labelVals, lp.GetValue())
		}
		desc := prometheus.NewDesc(name, help, labelNames, nil)
		if mc := constMetric(desc, fam.GetType(), m, labelVals); mc != nil {
			ch <- mc
		}
	}
}

// constMetric builds one prometheus.Metric from a scraped dto.Metric. Returns nil
// on an unsupported type or a reconstruction error (skipped, never panics).
func constMetric(desc *prometheus.Desc, t dto.MetricType, m *dto.Metric, labelVals []string) prometheus.Metric {
	var (
		mc  prometheus.Metric
		err error
	)
	switch t {
	case dto.MetricType_COUNTER:
		mc, err = prometheus.NewConstMetric(desc, prometheus.CounterValue, m.GetCounter().GetValue(), labelVals...)
	case dto.MetricType_GAUGE:
		mc, err = prometheus.NewConstMetric(desc, prometheus.GaugeValue, m.GetGauge().GetValue(), labelVals...)
	case dto.MetricType_UNTYPED:
		mc, err = prometheus.NewConstMetric(desc, prometheus.UntypedValue, m.GetUntyped().GetValue(), labelVals...)
	case dto.MetricType_HISTOGRAM:
		h := m.GetHistogram()
		buckets := make(map[float64]uint64, len(h.GetBucket()))
		for _, b := range h.GetBucket() {
			buckets[b.GetUpperBound()] = b.GetCumulativeCount()
		}
		mc, err = prometheus.NewConstHistogram(desc, h.GetSampleCount(), h.GetSampleSum(), buckets, labelVals...)
	case dto.MetricType_SUMMARY:
		s := m.GetSummary()
		q := make(map[float64]float64, len(s.GetQuantile()))
		for _, qq := range s.GetQuantile() {
			q[qq.GetQuantile()] = qq.GetValue()
		}
		mc, err = prometheus.NewConstSummary(desc, s.GetSampleCount(), s.GetSampleSum(), q, labelVals...)
	default:
		return nil
	}
	if err != nil {
		return nil
	}
	return mc
}
