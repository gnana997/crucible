package telemetry

import (
	"context"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
)

// Exporter is the seam for a telemetry sink. O-M1 defines the interface and the
// Provider lifecycle; the built-in exporters (Prometheus, OTLP, stdout) land in
// O-M2/O-M3. It is a compile-time interface, not a plugin loader — a new sink is
// a reviewed built-in, never foreign code loaded at runtime.
type Exporter interface {
	// Name identifies the exporter in logs (e.g. "otlp", "prometheus").
	Name() string
	// Shutdown flushes and stops the exporter, bounded by ctx.
	Shutdown(ctx context.Context) error
}

// Provider owns the daemon's telemetry identity (Resource) and the configured
// exporter set, and shuts them down cleanly on daemon exit.
type Provider struct {
	Resource  Resource
	exporters []Exporter
	log       *slog.Logger
}

// Config configures the Provider.
type Config struct {
	ServiceName string
	Logger      *slog.Logger

	// OTLP metric export (v0.5.4 O-M3). Off unless OTLPEndpoint or an
	// OTEL_EXPORTER_OTLP_* env var is set. Gatherer is the Prometheus registry
	// bridged to OTLP (the same series /metrics serves).
	OTLPEndpoint string
	OTLPProtocol string // "grpc" (default) or "http"
	OTLPHeaders  string // "k=v,k=v" (auth/tenant); SDK-parsed
	OTLPInsecure bool
	Gatherer     prometheus.Gatherer
}

// New builds the Provider. It never hard-fails: with no exporters configured it
// is an inert identity holder (zero-surprise — nothing exported unless asked),
// and an OTLP setup error is logged and skipped so the daemon still starts (and
// /metrics keeps serving regardless).
func New(ctx context.Context, cfg Config) *Provider {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	res := NewResource(cfg.ServiceName)
	p := &Provider{Resource: res, log: log.With("component", "telemetry")}
	p.log.Info("telemetry resource",
		"service.name", res.ServiceName,
		"service.version", res.ServiceVersion,
		"host.name", res.HostName)

	applyOTLPEnv(cfg)
	if otlpEnabled() {
		switch cfg.Gatherer {
		case nil:
			p.log.Warn("otlp metrics: no metrics registry supplied; export skipped")
		default:
			if om, err := newOTLPMetrics(ctx, res.otel(), cfg.Gatherer); err != nil {
				p.log.Warn("otlp metrics export disabled", "err", err)
			} else {
				p.Register(om)
				p.log.Info("otlp metrics export enabled",
					"endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
			}
		}
	}
	return p
}

// Register adds an exporter to the set (called by later milestones as they wire
// Prometheus/OTLP/stdout). Safe on a nil Provider.
func (p *Provider) Register(e Exporter) {
	if p == nil || e == nil {
		return
	}
	p.exporters = append(p.exporters, e)
	p.log.Info("telemetry exporter registered", "exporter", e.Name())
}

// Shutdown flushes and stops every exporter, bounded by ctx. Returns the first
// error; attempts them all regardless. Safe on a nil Provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var firstErr error
	for _, e := range p.exporters {
		if err := e.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
