package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// appOpsServer builds a Server over an app.Manager the test controls, so it can
// drive the manager into a state (unknown app / no instance / running instance)
// and exercise the /apps/{name}/exec|logs resolution.
func appOpsServer(t *testing.T) (*httptest.Server, *app.Manager) {
	t.Helper()
	store, err := app.Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	amgr := app.NewManager(store, &fakeInst{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv, err := New(Config{
		Manager:    stubSandboxManager(t),
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		AppManager: amgr,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, amgr
}

func status(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestAppOpsResolution covers the name→instance resolution the app-ops routes
// add: unknown app → 404, an app with no running instance → 409, and — once an
// instance is current — resolution succeeds and delegates (the sandbox logs
// handler, with no log store here, answers 501 — proving we got past resolution
// rather than 404/409).
func TestAppOpsResolution(t *testing.T) {
	ts, amgr := appOpsServer(t)

	// Unknown app → 404 on both routes.
	if got := status(t, http.MethodGet, ts.URL+"/apps/nope/logs"); got != http.StatusNotFound {
		t.Errorf("unknown app logs = %d, want 404", got)
	}
	if got := status(t, http.MethodPost, ts.URL+"/apps/nope/exec"); got != http.StatusNotFound {
		t.Errorf("unknown app exec = %d, want 404", got)
	}

	// An app with no running instance → 409.
	spec := api.AppSpec{
		Name:    "web",
		Image:   &api.ImageRef{OCI: "nginx:alpine"},
		Restart: wire.RestartPolicy{Policy: wire.RestartAlways},
	}
	if _, err := amgr.Create(spec, true); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := status(t, http.MethodGet, ts.URL+"/apps/web/logs"); got != http.StatusConflict {
		t.Errorf("no-instance logs = %d, want 409", got)
	}
	if got := status(t, http.MethodPost, ts.URL+"/apps/web/exec"); got != http.StatusConflict {
		t.Errorf("no-instance exec = %d, want 409", got)
	}

	// Reconcile once (Start runs an initial pass synchronously) so the fake
	// instantiator boots an instance and Status.InstanceID is set.
	if err := amgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(amgr.Stop)

	// Now resolution succeeds and delegates to the sandbox logs handler, which
	// answers 501 (no log store wired) — the point is it's NOT 404/409.
	if got := status(t, http.MethodGet, ts.URL+"/apps/web/logs"); got == http.StatusNotFound || got == http.StatusConflict {
		t.Errorf("resolvable app logs = %d, want resolution to succeed (delegated, not 404/409)", got)
	}
}

// TestAppOpsDisabled: no AppManager → 501 on the app-ops routes.
func TestAppOpsDisabled(t *testing.T) {
	ts, _ := newTestServer(t) // no AppManager wired
	if got := status(t, http.MethodGet, ts.URL+"/apps/web/logs"); got != http.StatusNotImplemented {
		t.Errorf("app logs with apps disabled = %d, want 501", got)
	}
	if got := status(t, http.MethodPost, ts.URL+"/apps/web/exec"); got != http.StatusNotImplemented {
		t.Errorf("app exec with apps disabled = %d, want 501", got)
	}
}
