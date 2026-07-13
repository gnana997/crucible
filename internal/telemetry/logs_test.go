package telemetry

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/logstore"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
)

// TestOTLPLogsPump proves an appended log-store record flows through the fanout
// → pump → OTel log pipeline → exporter, carrying the instance id and stream as
// attributes (the O-M3b path, with a stdout exporter standing in for OTLP).
func TestOTLPLogsPump(t *testing.T) {
	store, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	exp, err := stdoutlog.New(stdoutlog.WithWriter(&buf))
	if err != nil {
		t.Fatalf("stdout log exporter: %v", err)
	}
	ol := newLogsPump(NewResource("logs-test").otel(), store, exp)
	t.Cleanup(func() { _ = ol.Shutdown(context.Background()) })

	if err := store.Append("sbx_x", logstore.Record{
		TimeMs: 100, Source: logstore.SourceService, Stream: logstore.StreamStderr, Text: "log-line-42",
	}); err != nil {
		t.Fatal(err)
	}

	// The pump is async; poll (flush + check) until the record shows up.
	ctx := context.Background()
	var out string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = ol.lp.ForceFlush(ctx)
		out = buf.String()
		if strings.Contains(out, "log-line-42") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "log-line-42") {
		t.Fatalf("log body not exported:\n%s", out)
	}
	if !strings.Contains(out, "crucible.app.instance") || !strings.Contains(out, "sbx_x") {
		t.Errorf("instance attribute missing:\n%s", out)
	}
	if !strings.Contains(out, "logs-test") {
		t.Errorf("resource service.name missing:\n%s", out)
	}
}
