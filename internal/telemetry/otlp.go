package telemetry

import (
	"context"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// applyOTLPEnv folds the OTLP flags into the OTEL_EXPORTER_OTLP_* environment the
// OTel SDK reads natively, so all endpoint/protocol/header/TLS parsing is the
// SDK's (mature, spec-compliant) and a flag simply overrides the env. Called
// once at startup (single-threaded).
func applyOTLPEnv(cfg Config) {
	setIf := func(k, v string) {
		if v != "" {
			_ = os.Setenv(k, v)
		}
	}
	setIf("OTEL_EXPORTER_OTLP_ENDPOINT", cfg.OTLPEndpoint)
	setIf("OTEL_EXPORTER_OTLP_PROTOCOL", cfg.OTLPProtocol)
	setIf("OTEL_EXPORTER_OTLP_HEADERS", cfg.OTLPHeaders) // SDK parses "k=v,k=v"
	if cfg.OTLPInsecure {
		_ = os.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	}
}

// otlpEnabled reports whether an OTLP endpoint is configured (via a flag folded
// into env by applyOTLPEnv, or a pre-existing OTEL_* env var).
func otlpEnabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

// otlpMetrics wraps the OTLP metric MeterProvider as an Exporter so the Provider
// can flush + stop it on shutdown.
type otlpMetrics struct{ mp *sdkmetric.MeterProvider }

func (o *otlpMetrics) Name() string                       { return "otlp-metrics" }
func (o *otlpMetrics) Shutdown(ctx context.Context) error { return o.mp.Shutdown(ctx) }

// newOTLPMetrics builds the OTLP metric pipeline: an OTLP exporter fed by a
// PeriodicReader that pulls the Prometheus registry via the OTel bridge — so the
// exact same /metrics series are pushed over OTLP with no metric redefinition
// and no change to the scrape endpoint.
func newOTLPMetrics(ctx context.Context, res *sdkresource.Resource, gatherer prometheus.Gatherer) (*otlpMetrics, error) {
	exp, err := newOTLPMetricExporter(ctx)
	if err != nil {
		return nil, err
	}
	return &otlpMetrics{mp: newMetricProvider(res, exp, gatherer)}, nil
}

// newMetricProvider assembles a MeterProvider whose PeriodicReader pulls the
// Prometheus registry (via the bridge) and pushes to exp. Shared by the OTLP
// path and tests (which inject a stdout/buffer exporter).
func newMetricProvider(res *sdkresource.Resource, exp sdkmetric.Exporter, gatherer prometheus.Gatherer) *sdkmetric.MeterProvider {
	reader := sdkmetric.NewPeriodicReader(exp,
		sdkmetric.WithProducer(prombridge.NewMetricProducer(prombridge.WithGatherer(gatherer))))
	return sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader))
}

// newOTLPMetricExporter builds the gRPC (default) or HTTP OTLP metric exporter,
// reading endpoint/headers/TLS from the OTEL_EXPORTER_OTLP_* env the SDK honors.
func newOTLPMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	proto := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if proto == "" {
		proto = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if proto == "http" || proto == "http/protobuf" {
		return otlpmetrichttp.New(ctx)
	}
	return otlpmetricgrpc.New(ctx)
}
