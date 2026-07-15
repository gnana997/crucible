package telemetry

import (
	"context"

	"github.com/gnana997/crucible/internal/appevents"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// StartEventExport streams app lifecycle events over OTLP as OTel log records.
// No-op unless OTLP is configured. Mirrors StartLogExport: it subscribes to the
// event store's fanout (best-effort, drop-on-full — never blocks the app) and
// runs a pump goroutine the Provider shuts down with the rest.
func (p *Provider) StartEventExport(ctx context.Context, store *appevents.Store) {
	if p == nil || store == nil || !otlpEnabled() {
		return
	}
	exp, err := newOTLPLogExporter(ctx)
	if err != nil {
		p.log.Warn("otlp events export disabled", "err", err)
		return
	}
	p.Register(newEventsPump(p.Resource.otel(), store, exp))
	p.log.Info("otlp events export enabled")
}

// newEventsPump builds a LoggerProvider around exp and pumps store → OTel.
// Returns the same wrapper the log pump uses (LoggerProvider + subscription).
func newEventsPump(res *sdkresource.Resource, store *appevents.Store, exp sdklog.Exporter) *otlpLogs {
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	logger := lp.Logger("crucible.events")
	ch, unsub := store.Subscribe(4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range ch {
			emitEvent(logger, e)
		}
	}()
	return &otlpLogs{lp: lp, unsub: unsub, done: done}
}

// emitEvent converts one lifecycle event to an OTel log record: the body is the
// event type; app/instance/seq and (for phase changes) from/to become attributes.
func emitEvent(logger otellog.Logger, e appevents.AppEvent) {
	var r otellog.Record
	r.SetBody(otellog.StringValue(e.Type))
	if !e.Time.IsZero() {
		r.SetTimestamp(e.Time)
	}
	r.SetSeverity(otellog.SeverityInfo)
	attrs := []otellog.KeyValue{
		otellog.String("crucible.app", e.App),
		otellog.String("crucible.app.id", e.AppID),
		otellog.String("event.type", e.Type),
		otellog.Int64("event.seq", int64(e.Seq)),
	}
	if e.Instance != "" {
		attrs = append(attrs, otellog.String(OTelAppInstance, e.Instance))
	}
	if from, ok := e.Attrs["from"].(string); ok {
		attrs = append(attrs, otellog.String("phase.from", from))
	}
	if to, ok := e.Attrs["to"].(string); ok {
		attrs = append(attrs, otellog.String("phase.to", to))
	}
	r.AddAttributes(attrs...)
	logger.Emit(context.Background(), r)
}
