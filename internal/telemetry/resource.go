package telemetry

import (
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/gnana997/crucible/internal/version"
)

// Resource identifies this crucible daemon in every exported signal — the OTel
// Resource (service.name / service.version / host.name) a backend groups
// metrics, logs, and traces under. Extra carries operator-supplied attributes
// (region, env, …) so dimensions can be added without a code change.
type Resource struct {
	ServiceName    string
	ServiceVersion string
	HostName       string
	Extra          map[string]string
}

// NewResource builds the daemon's telemetry identity. serviceName falls back to
// OTEL_SERVICE_NAME, then "crucible"; version comes from the build; host.name
// from the OS. OTEL_RESOURCE_ATTRIBUTES ("k=v,k=v") is parsed into Extra — the
// same env var the OpenTelemetry SDK reads, so an operator already running OTel
// gets no surprises.
func NewResource(serviceName string) Resource {
	if serviceName == "" {
		serviceName = os.Getenv("OTEL_SERVICE_NAME")
	}
	if serviceName == "" {
		serviceName = "crucible"
	}
	host, _ := os.Hostname()
	return Resource{
		ServiceName:    serviceName,
		ServiceVersion: version.String(),
		HostName:       host,
		Extra:          parseResourceAttrs(os.Getenv("OTEL_RESOURCE_ATTRIBUTES")),
	}
}

// otel maps the Resource to an OpenTelemetry *resource.Resource for the OTLP
// pipeline: service.name/version, host.name, and any OTEL_RESOURCE_ATTRIBUTES.
func (r Resource) otel() *sdkresource.Resource {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", r.ServiceName),
		attribute.String("service.version", r.ServiceVersion),
	}
	if r.HostName != "" {
		attrs = append(attrs, attribute.String("host.name", r.HostName))
	}
	for k, v := range r.Extra {
		attrs = append(attrs, attribute.String(k, v))
	}
	return sdkresource.NewSchemaless(attrs...)
}

func parseResourceAttrs(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok {
			k = strings.TrimSpace(k)
			if k != "" {
				out[k] = strings.TrimSpace(v)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
