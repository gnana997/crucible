package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gnana997/crucible/internal/app"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// stubSandboxManager builds a real sandbox.Manager over a stub runner so
// daemon.New's required Manager field is satisfied; the /apps routes never
// touch it (the fake app.Instantiator stands in for instance creation).
func stubSandboxManager(t *testing.T) *sandbox.Manager {
	t.Helper()
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("fake"), 0o640); err != nil {
		t.Fatal(err)
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
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	return mgr
}

// fakeInst is a no-op app.Instantiator so the route tests exercise the
// HTTP surface + manager without booting real VMs.
type fakeInst struct{ n int }

func (f *fakeInst) Create(_ context.Context, appID string, _ api.AppSpec) (string, error) {
	f.n++
	return "sbx_" + appID, nil
}
func (f *fakeInst) Exists(string) bool                    { return true }
func (f *fakeInst) Destroy(context.Context, string) error { return nil }
func (f *fakeInst) Sleep(context.Context, string) error   { return nil }
func (f *fakeInst) Wake(context.Context, string) error    { return nil }
func (f *fakeInst) Probe(context.Context, string, api.HealthCheck) app.Health {
	return app.HealthPassing
}
func (f *fakeInst) ImageHealth(context.Context, api.AppSpec) (*api.HealthCheck, error) {
	return nil, nil
}

// newAppTestServer builds a Server whose only wired dependency is the app
// manager (Manager is set to a throwaway to satisfy New's required field).
func newAppTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := app.Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	amgr := app.NewManager(store, &fakeInst{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	srv, err := New(Config{
		Manager:    stubSandboxManager(t), // required; unused by /apps routes
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		AppManager: amgr,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestAppRoutesCRUD(t *testing.T) {
	ts := newAppTestServer(t)

	// Create.
	body, _ := json.Marshal(api.CreateAppRequest{AppSpec: api.AppSpec{
		Name:    "web",
		Image:   &api.ImageRef{OCI: "nginx:alpine"},
		Restart: wire.RestartPolicy{Policy: wire.RestartAlways},
	}})
	resp, err := http.Post(ts.URL+"/apps", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create app = %d: %s", resp.StatusCode, b)
	}
	var created api.AppResponse
	_ = json.NewDecoder(resp.Body).Decode(&created)
	_ = resp.Body.Close()
	if !app.IsValidID(created.ID) || created.Name != "web" || created.DesiredState != "running" {
		t.Fatalf("created = %+v", created)
	}

	// Duplicate name → 409.
	resp, _ = http.Post(ts.URL+"/apps", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Bad spec (no image) → 400.
	bad, _ := json.Marshal(api.CreateAppRequest{AppSpec: api.AppSpec{Name: "noimg"}})
	resp, _ = http.Post(ts.URL+"/apps", "application/json", bytes.NewReader(bad))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no-image app = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Get by name.
	resp, _ = http.Get(ts.URL + "/apps/web")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("get app = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Get missing → 404.
	resp, _ = http.Get(ts.URL + "/apps/ghost")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// List.
	resp, _ = http.Get(ts.URL + "/apps")
	var list api.AppListResponse
	_ = json.NewDecoder(resp.Body).Decode(&list)
	_ = resp.Body.Close()
	if len(list.Apps) != 1 {
		t.Errorf("list = %d apps, want 1", len(list.Apps))
	}

	// Delete.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apps/web", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/apps/web")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("after delete, get = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestAppRoutesDisabled: no AppManager → 501.
func TestAppRoutesDisabled(t *testing.T) {
	srv, err := New(Config{
		Manager: stubSandboxManager(t),
		Addr:    "127.0.0.1:0",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, _ := http.Get(ts.URL + "/apps")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("apps disabled = %d, want 501", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
