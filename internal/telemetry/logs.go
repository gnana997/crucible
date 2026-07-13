package telemetry

import (
	"context"
	"os"
	"time"

	"github.com/gnana997/crucible/internal/logstore"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// StartLogExport streams the durable app logs (from the log store) over OTLP as
// OTel log records. No-op unless OTLP is configured (endpoint via flag/env). It
// subscribes to the store's fanout (best-effort, drop-on-full — never blocks the
// app) and runs a pump goroutine; the Provider shuts it down with the rest.
func (p *Provider) StartLogExport(ctx context.Context, store *logstore.Store) {
	if p == nil || store == nil || !otlpEnabled() {
		return
	}
	if ol, err := newOTLPLogs(ctx, p.Resource.otel(), store); err != nil {
		p.log.Warn("otlp logs export disabled", "err", err)
	} else {
		p.Register(ol)
		p.log.Info("otlp logs export enabled")
	}
}

// otlpLogs wraps the LoggerProvider + the log-store pump goroutine.
type otlpLogs struct {
	lp    *sdklog.LoggerProvider
	unsub func()
	done  chan struct{}
}

func (o *otlpLogs) Name() string { return "otlp-logs" }

func (o *otlpLogs) Shutdown(ctx context.Context) error {
	o.unsub() // close the subscription → the pump drains and exits
	select {
	case <-o.done:
	case <-ctx.Done():
	}
	return o.lp.Shutdown(ctx) // flush any batched records
}

func newOTLPLogs(ctx context.Context, res *sdkresource.Resource, store *logstore.Store) (*otlpLogs, error) {
	exp, err := newOTLPLogExporter(ctx)
	if err != nil {
		return nil, err
	}
	return newLogsPump(res, store, exp), nil
}

// newLogsPump builds the LoggerProvider around exp and starts the store→OTel
// pump goroutine. Shared by the OTLP path and tests (which inject a stdout
// exporter).
func newLogsPump(res *sdkresource.Resource, store *logstore.Store, exp sdklog.Exporter) *otlpLogs {
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	logger := lp.Logger("crucible")
	ch, unsub := store.Subscribe(4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			emitLog(logger, ev)
		}
	}()
	return &otlpLogs{lp: lp, unsub: unsub, done: done}
}

// emitLog converts one log-store record to an OTel log record. The store keys by
// instance (sandbox) id → crucible.app.instance; source/stream become attributes.
func emitLog(logger otellog.Logger, ev logstore.Event) {
	var r otellog.Record
	r.SetBody(otellog.StringValue(ev.Rec.Text))
	if ev.Rec.TimeMs > 0 {
		r.SetTimestamp(time.UnixMilli(ev.Rec.TimeMs))
	}
	sev := otellog.SeverityInfo
	if ev.Rec.Stream == logstore.StreamStderr {
		sev = otellog.SeverityWarn
	}
	r.SetSeverity(sev)
	r.AddAttributes(
		otellog.String(OTelAppInstance, ev.ID),
		otellog.String("log.source", ev.Rec.Source),
		otellog.String("log.stream", ev.Rec.Stream),
	)
	logger.Emit(context.Background(), r)
}

func newOTLPLogExporter(ctx context.Context) (sdklog.Exporter, error) {
	proto := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL")
	if proto == "" {
		proto = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if proto == "http" || proto == "http/protobuf" {
		return otlploghttp.New(ctx)
	}
	return otlploggrpc.New(ctx)
}
