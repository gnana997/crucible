package telemetry

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
)

// TestOTLPMetricBridge proves a client_golang registry flows through the OTel
// Prometheus bridge → metric pipeline → exporter with no metric redefinition —
// the core of OTLP metric export: the same /metrics series pushed over OTLP.
func TestOTLPMetricBridge(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "demo_widget_total", Help: "x"})
	reg.MustRegister(c)
	c.Add(3)

	var buf bytes.Buffer
	exp, err := stdoutmetric.New(stdoutmetric.WithWriter(&buf))
	if err != nil {
		t.Fatalf("stdout exporter: %v", err)
	}
	mp := newMetricProvider(NewResource("test-svc").otel(), exp, reg)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	if err := mp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "demo_widget_total") {
		t.Errorf("bridged prometheus metric was not exported over the OTel pipeline:\n%s", out)
	}
	if !strings.Contains(out, "test-svc") {
		t.Errorf("resource service.name missing from export:\n%s", out)
	}
}

func TestApplyOTLPEnvAndEnabled(t *testing.T) {
	// Contain env mutation: t.Setenv restores these after the test even though
	// applyOTLPEnv writes them via os.Setenv.
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_INSECURE",
	} {
		t.Setenv(k, "")
	}
	if otlpEnabled() {
		t.Fatal("otlpEnabled should be false with no endpoint")
	}
	applyOTLPEnv(Config{OTLPEndpoint: "http://collector:4317", OTLPHeaders: "authorization=Bearer x", OTLPInsecure: true})
	if !otlpEnabled() {
		t.Error("otlpEnabled should be true after applyOTLPEnv set the endpoint")
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_HEADERS") != "authorization=Bearer x" {
		t.Errorf("headers env = %q", os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"))
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") != "true" {
		t.Error("insecure env not set")
	}
}
