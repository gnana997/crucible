package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
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

func (h *stubHandle) Workdir() string                               { return h.workdir }
func (h *stubHandle) VSockPath() string                             { return "" }
func (h *stubHandle) Pause(context.Context) error                   { return nil }
func (h *stubHandle) Resume(context.Context) error                  { return nil }
func (h *stubHandle) PatchRootfs(_ context.Context, _ string) error { return nil }
func (h *stubHandle) Snapshot(_ context.Context, statePath, memPath string) error {
	_ = os.WriteFile(statePath, []byte("stub-state"), 0o640)
	_ = os.WriteFile(memPath, []byte("stub-memory"), 0o640)
	return nil
}
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

func (r *stubRunner) Restore(_ context.Context, spec runner.RestoreSpec) (runner.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

	tmpl := t.TempDir() + "/rootfs.ext4"
	if err := os.WriteFile(tmpl, []byte("fake-template"), 0o640); err != nil {
		t.Fatalf("write template rootfs: %v", err)
	}

	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:   &stubRunner{},
		WorkBase: workBase,
		Kernel:   "/fake/vmlinux",
		Rootfs:   tmpl,
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

func TestCreateSandboxImageRefRejectsBothSet(t *testing.T) {
	ts, _ := newTestServer(t)

	body := bytes.NewBufferString(`{"image":{"path":"/a.ext4","oci":"ghcr.io/x/y:1"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (mutually exclusive)", resp.StatusCode)
	}
	var e errorResponse
	decodeJSON(t, resp, &e)
	if !strings.Contains(e.Error, "mutually exclusive") {
		t.Errorf("error = %q, want 'mutually exclusive' substring", e.Error)
	}
}

func TestCreateSandboxImageRefRejectsEmpty(t *testing.T) {
	ts, _ := newTestServer(t)

	body := bytes.NewBufferString(`{"image":{}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (must set one)", resp.StatusCode)
	}
}

func TestCreateSandboxImageRefOCIReturns501(t *testing.T) {
	ts, _ := newTestServer(t)

	body := bytes.NewBufferString(`{"image":{"oci":"ghcr.io/x/y:1"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	var e errorResponse
	decodeJSON(t, resp, &e)
	if !strings.Contains(e.Error, "not implemented") {
		t.Errorf("error = %q, want 'not implemented' substring", e.Error)
	}
}

func TestCreateSandboxImageRefPathReturns501(t *testing.T) {
	ts, _ := newTestServer(t)

	body := bytes.NewBufferString(`{"image":{"path":"/a.ext4"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (per-sandbox path override not implemented in v0.1)", resp.StatusCode)
	}
}

// Image field absent → sandbox created normally with daemon default
// rootfs. Locks in the wire-compat promise: adding the field must not
// break existing clients that omit it.
func TestCreateSandboxImageRefAbsentStillWorks(t *testing.T) {
	ts, _ := newTestServer(t)

	body := bytes.NewBufferString(`{"vcpus":2}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
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

func TestExecSandboxRouteValidatesID(t *testing.T) {
	ts, _ := newTestServer(t)
	body := strings.NewReader(`{"cmd":["/bin/true"]}`)
	resp, err := http.Post(ts.URL+"/sandboxes/not-a-real-id/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestExecSandboxRouteSandboxNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	body := strings.NewReader(`{"cmd":["/bin/true"]}`)
	// Valid-looking ID, but we haven't created it.
	resp, err := http.Post(ts.URL+"/sandboxes/sbx_0000000000000/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestExecSandboxRouteRejectsEmptyCmd(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create a sandbox first so the 400 check runs after the 404 check.
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created sandboxResponse
	decodeJSON(t, resp, &created)

	body := strings.NewReader(`{"cmd":[]}`)
	resp, err = http.Post(ts.URL+"/sandboxes/"+created.ID+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST /exec: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestExecSandboxRouteStubAgentSynthesizesExitFrame(t *testing.T) {
	// The stub runner creates sandboxes with no vsock path, so
	// Manager.Exec fails with a "no agent vsock path" error. The
	// daemon must commit to a 200 + streamed body anyway and
	// synthesize a terminal FrameExit that surfaces the error.
	ts, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created sandboxResponse
	decodeJSON(t, resp, &created)

	body := strings.NewReader(`{"cmd":["/bin/true"]}`)
	resp, err = http.Post(ts.URL+"/sandboxes/"+created.ID+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST /exec: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}

	// Read frames; expect exactly one FrameExit with Error populated.
	var result agentwire.ExecResult
	sawExit := false
	for {
		f, err := agentwire.ReadFrame(resp.Body)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if f.Type == agentwire.FrameExit {
			if err := json.Unmarshal(f.Payload, &result); err != nil {
				t.Fatalf("decode exit: %v", err)
			}
			sawExit = true
		}
	}
	if !sawExit {
		t.Fatal("no exit frame in response")
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.Error == "" {
		t.Error("Error = empty, want populated")
	}
}

func TestCreateSandboxTimeoutPassedThrough(t *testing.T) {
	// We can't easily observe the lifetime timer from the HTTP layer
	// (it fires in a goroutine), so just verify the field is accepted
	// and the sandbox is created normally. Detailed timer behavior is
	// covered in sandbox/sandbox_test.go.
	ts, _ := newTestServer(t)
	body := strings.NewReader(`{"timeout_s": 60}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}

// --- snapshot / fork routes ------------------------------------------

func createSandboxViaHTTP(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", nil)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	var out sandboxResponse
	decodeJSON(t, resp, &out)
	return out.ID
}

func createSnapshotViaHTTP(t *testing.T, ts *httptest.Server, sbxID string) snapshotResponse {
	t.Helper()
	resp, err := http.Post(ts.URL+"/sandboxes/"+sbxID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("POST snapshot: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("snapshot status = %d, body = %s", resp.StatusCode, body)
	}
	var out snapshotResponse
	decodeJSON(t, resp, &out)
	return out
}

func TestCreateSnapshotHappyPath(t *testing.T) {
	ts, _ := newTestServer(t)
	sbxID := createSandboxViaHTTP(t, ts)
	snap := createSnapshotViaHTTP(t, ts, sbxID)

	if !strings.HasPrefix(snap.ID, "snap_") {
		t.Errorf("snapshot id %q: missing snap_ prefix", snap.ID)
	}
	if snap.SourceID != sbxID {
		t.Errorf("SourceID = %q, want %q", snap.SourceID, sbxID)
	}
}

func TestCreateSnapshotSandboxNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/sandboxes/sbx_0000000000000/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCreateSnapshotInvalidID(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/sandboxes/not-a-real-id/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestListSnapshots(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/snapshots")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var empty snapshotListResponse
	decodeJSON(t, resp, &empty)
	if len(empty.Snapshots) != 0 {
		t.Errorf("expected empty list, got %d", len(empty.Snapshots))
	}

	sbxID := createSandboxViaHTTP(t, ts)
	_ = createSnapshotViaHTTP(t, ts, sbxID)
	_ = createSnapshotViaHTTP(t, ts, sbxID)

	resp, err = http.Get(ts.URL + "/snapshots")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var got snapshotListResponse
	decodeJSON(t, resp, &got)
	if len(got.Snapshots) != 2 {
		t.Errorf("len = %d, want 2", len(got.Snapshots))
	}
}

func TestGetSnapshot(t *testing.T) {
	ts, _ := newTestServer(t)
	sbxID := createSandboxViaHTTP(t, ts)
	snap := createSnapshotViaHTTP(t, ts, sbxID)

	resp, err := http.Get(ts.URL + "/snapshots/" + snap.ID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got snapshotResponse
	decodeJSON(t, resp, &got)
	if got.ID != snap.ID {
		t.Errorf("ID = %q, want %q", got.ID, snap.ID)
	}
}

func TestGetSnapshotNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/snapshots/snap_0000000000000")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteSnapshot(t *testing.T) {
	ts, _ := newTestServer(t)
	sbxID := createSandboxViaHTTP(t, ts)
	snap := createSnapshotViaHTTP(t, ts, sbxID)

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/snapshots/"+snap.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	// Second delete should 404.
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE again: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second DELETE status = %d, want 404", resp.StatusCode)
	}
}

func TestForkSnapshot(t *testing.T) {
	ts, _ := newTestServer(t)
	sbxID := createSandboxViaHTTP(t, ts)
	snap := createSnapshotViaHTTP(t, ts, sbxID)

	resp, err := http.Post(ts.URL+"/snapshots/"+snap.ID+"/fork?count=3", "application/json", nil)
	if err != nil {
		t.Fatalf("POST fork: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got forkResponse
	decodeJSON(t, resp, &got)
	if len(got.Sandboxes) != 3 {
		t.Errorf("len = %d, want 3", len(got.Sandboxes))
	}
	for _, s := range got.Sandboxes {
		if !strings.HasPrefix(s.ID, "sbx_") {
			t.Errorf("fork id %q: missing sbx_ prefix", s.ID)
		}
	}
}

func TestForkSnapshotDefaultsCountToOne(t *testing.T) {
	ts, _ := newTestServer(t)
	sbxID := createSandboxViaHTTP(t, ts)
	snap := createSnapshotViaHTTP(t, ts, sbxID)

	resp, err := http.Post(ts.URL+"/snapshots/"+snap.ID+"/fork", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var got forkResponse
	decodeJSON(t, resp, &got)
	if len(got.Sandboxes) != 1 {
		t.Errorf("len = %d, want 1", len(got.Sandboxes))
	}
}

func TestForkSnapshotRejectsBadCount(t *testing.T) {
	ts, _ := newTestServer(t)
	sbxID := createSandboxViaHTTP(t, ts)
	snap := createSnapshotViaHTTP(t, ts, sbxID)

	for _, q := range []string{"0", "-1", "not-a-number"} {
		t.Run("count="+q, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/snapshots/"+snap.ID+"/fork?count="+q, "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestForkSnapshotNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/snapshots/snap_0000000000000/fork?count=1", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
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
