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
	"strings"
	"sync"
	"testing"

	"github.com/gnana997/crucible/internal/runner"
	"github.com/gnana997/crucible/internal/sandbox"
)

// --- test stubs -------------------------------------------------------

type stubHandle struct {
	workdir  string
	shutdown chan struct{}
}

func newStubHandle(workdir string) *stubHandle {
	return &stubHandle{workdir: workdir, shutdown: make(chan struct{})}
}

func (h *stubHandle) Workdir() string { return h.workdir }
func (h *stubHandle) Shutdown(context.Context) error {
	select {
	case <-h.shutdown:
	default:
		close(h.shutdown)
	}
	return nil
}
func (h *stubHandle) Wait() error { <-h.shutdown; return nil }

type stubRunner struct {
	mu    sync.Mutex
	calls []runner.Spec
}

func (r *stubRunner) Start(_ context.Context, spec runner.Spec) (runner.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, spec)
	_ = os.MkdirAll(spec.Workdir, 0o755)
	return newStubHandle(spec.Workdir), nil
}

// --- test harness -----------------------------------------------------

// newTestServer builds a Server wired to a real Manager + stub runner +
// a silent logger, returns the httptest.Server (for client URLs) and the
// Manager (for assertions).
func newTestServer(t *testing.T) (*httptest.Server, *sandbox.Manager) {
	t.Helper()
	workBase := t.TempDir()
	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:   &stubRunner{},
		WorkBase: workBase,
		Kernel:   "/fake/vmlinux",
		Rootfs:   "/fake/rootfs.ext4",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Discard log output in tests so `go test -v` stays readable.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := New(Config{
		Manager: mgr,
		Addr:    "127.0.0.1:0",
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, mgr
}

// decodeJSON asserts the response carries a JSON body and decodes it.
func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// --- route tests ------------------------------------------------------

func TestHealthz(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	decodeJSON(t, resp, &body)
	if body["status"] != "ok" {
		t.Errorf(`body["status"] = %q, want "ok"`, body["status"])
	}
}

func TestCreateSandboxDefaults(t *testing.T) {
	ts, _ := newTestServer(t)

	// Empty body should be accepted and defaults filled in.
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got sandboxResponse
	decodeJSON(t, resp, &got)
	if !strings.HasPrefix(got.ID, "sbx_") {
		t.Errorf("ID = %q, want sbx_ prefix", got.ID)
	}
	if got.VCPUs != sandbox.DefaultVCPUs || got.MemoryMiB != sandbox.DefaultMemoryMiB {
		t.Errorf("defaults not applied: VCPUs=%d MemoryMiB=%d", got.VCPUs, got.MemoryMiB)
	}
}

func TestCreateSandboxWithBody(t *testing.T) {
	ts, _ := newTestServer(t)

	body := bytes.NewBufferString(`{"vcpus": 4, "memory_mib": 2048}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var got sandboxResponse
	decodeJSON(t, resp, &got)
	if got.VCPUs != 4 || got.MemoryMiB != 2048 {
		t.Errorf("got %+v", got)
	}
}

func TestCreateSandboxBadJSON(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var e errorResponse
	decodeJSON(t, resp, &e)
	if !strings.Contains(e.Error, "invalid json") {
		t.Errorf("error = %q, want substring 'invalid json'", e.Error)
	}
}

func TestGetSandbox(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var created sandboxResponse
	decodeJSON(t, resp, &created)

	resp, err = http.Get(ts.URL + "/sandboxes/" + created.ID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got sandboxResponse
	decodeJSON(t, resp, &got)
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
}

func TestGetSandboxNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	// Valid-looking ID, just one that doesn't exist.
	resp, err := http.Get(ts.URL + "/sandboxes/sbx_0000000000000")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetSandboxInvalidID(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/sandboxes/not-a-real-id")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestListSandboxes(t *testing.T) {
	ts, _ := newTestServer(t)

	// Initially empty.
	resp, err := http.Get(ts.URL + "/sandboxes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var empty listResponse
	decodeJSON(t, resp, &empty)
	if len(empty.Sandboxes) != 0 {
		t.Errorf("expected empty list, got %d", len(empty.Sandboxes))
	}

	// Create two; list should return two.
	for i := 0; i < 2; i++ {
		if _, err := http.Post(ts.URL+"/sandboxes", "application/json", nil); err != nil {
			t.Fatalf("POST: %v", err)
		}
	}
	resp, err = http.Get(ts.URL + "/sandboxes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var got listResponse
	decodeJSON(t, resp, &got)
	if len(got.Sandboxes) != 2 {
		t.Errorf("len = %d, want 2", len(got.Sandboxes))
	}
}

func TestDeleteSandbox(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var created sandboxResponse
	decodeJSON(t, resp, &created)

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+created.ID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	// Second delete: 404.
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE again: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts, _ := newTestServer(t)
	// GET on POST-only route should yield 405 thanks to the method-aware
	// pattern "POST /sandboxes" — Go's ServeMux tracks which methods are
	// registered and rejects the others automatically.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no manager", Config{Addr: "127.0.0.1:0"}},
		{"no addr", Config{Manager: &sandbox.Manager{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("New: got nil, want error")
			}
		})
	}
}
