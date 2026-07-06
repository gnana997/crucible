package daemon

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/metrics"
	"github.com/gnana997/crucible/internal/sandbox"
)

// serverWithMetrics builds a Server whose Config.Metrics is mx (which may
// be nil, to exercise the disabled-endpoint path).
func serverWithMetrics(t *testing.T, mx *metrics.Metrics) *Server {
	t.Helper()
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("fake-template"), 0o640); err != nil {
		t.Fatalf("write template rootfs: %v", err)
	}
	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:   &stubRunner{t: t},
		WorkBase: t.TempDir(),
		Kernel:   "/fake/vmlinux",
		Rootfs:   tmpl,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv, err := New(Config{
		Manager: mgr,
		Addr:    "127.0.0.1:0",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: mx,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return srv
}

func TestMetricsRouteServedWhenConfigured(t *testing.T) {
	srv := serverWithMetrics(t, metrics.New())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "sandboxes_created_total") {
		t.Errorf("/metrics body missing expected series:\n%s", body)
	}
}

func TestMetricsRoute404WhenNil(t *testing.T) {
	srv := serverWithMetrics(t, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 404 {
		t.Fatalf("GET /metrics with nil Metrics = %d, want 404", rec.Code)
	}
}
