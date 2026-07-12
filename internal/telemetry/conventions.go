// Package telemetry is crucible's observability export seam: the daemon's
// telemetry identity (Resource), the shared per-app attribute/label vocabulary,
// and a Provider that owns the configured exporters and shuts them down cleanly.
//
// Design (see the v0.5.4 plan): crucible is a telemetry *source*, not a
// pipeline. It emits open standards — Prometheus /metrics and (from O-M3) OTLP —
// and delegates routing/fan-out to the ecosystem (OpenTelemetry Collector,
// Vector, Grafana Alloy). The Exporter interface here is a compile-time seam,
// NOT a runtime plugin loader: adding a sink means adding a reviewed built-in,
// never loading foreign code.
package telemetry

// Prometheus label names (short, scrape-friendly), kept to a fixed,
// low-cardinality set — app / instance / status-class / phase only, never a raw
// path, method, or client IP.
const (
	LabelApp      = "app"
	LabelInstance = "instance"
	LabelCode     = "code" // HTTP status class: 1xx…5xx (see StatusClass)
	LabelPhase    = "phase"
)

// OpenTelemetry attribute keys (namespaced), used on the OTLP resource and
// per-signal attributes so metrics, logs, and traces line up on a backend.
const (
	OTelAppName       = "crucible.app.name"
	OTelAppInstance   = "crucible.app.instance"
	OTelAppGeneration = "crucible.app.generation"
	OTelImageDigest   = "crucible.image.digest"
)

// StatusClass maps an HTTP status code to its bounded label value (1xx…5xx),
// keeping request-metric cardinality fixed regardless of the exact code.
func StatusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}
