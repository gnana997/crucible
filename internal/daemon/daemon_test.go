package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/runner"
	"github.com/gnana997/crucible/internal/sandbox"
)

// --- test stubs -------------------------------------------------------

type stubHandle struct {
	workdir   string
	vsock     string // agent UDS path; empty for cold-boot handles
	shutdown  chan struct{}
	snapDelay time.Duration // stalls Snapshot to model a large-guest write
}

func newStubHandle(workdir string) *stubHandle {
	return &stubHandle{workdir: workdir, shutdown: make(chan struct{})}
}

func (h *stubHandle) Workdir() string                               { return h.workdir }
func (h *stubHandle) VSockPath() string                             { return h.vsock }
func (h *stubHandle) Pause(context.Context) error                   { return nil }
func (h *stubHandle) Resume(context.Context) error                  { return nil }
func (h *stubHandle) PatchRootfs(_ context.Context, _ string) error { return nil }
func (h *stubHandle) Snapshot(_ context.Context, statePath, memPath string) error {
	if h.snapDelay > 0 {
		time.Sleep(h.snapDelay)
	}
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
	// t, when set, makes Restore serve a stub agent behind the handle's
	// vsock so the fork path's clone-safety refresh (now fatal without an
	// agent channel) succeeds.
	t            *testing.T
	snapDelay    time.Duration // stamped onto handles to stall their Snapshot
	restoreDelay time.Duration // stalls Restore to model a large-guest load
	mu           sync.Mutex
	calls        []runner.Spec
}

func (r *stubRunner) Start(_ context.Context, spec runner.Spec) (runner.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, spec)
	_ = os.MkdirAll(spec.Workdir, 0o755)
	h := newStubHandle(spec.Workdir)
	h.snapDelay = r.snapDelay
	return h, nil
}

func (r *stubRunner) Restore(_ context.Context, spec runner.RestoreSpec) (runner.Handle, error) {
	if r.restoreDelay > 0 {
		time.Sleep(r.restoreDelay) // model a large-guest load
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = os.MkdirAll(spec.Workdir, 0o755)
	h := newStubHandle(spec.Workdir)
	if r.t != nil {
		sock := filepath.Join(spec.Workdir, "a.sock")
		serveStubAgent(r.t, sock)
		h.vsock = sock
	}
	return h, nil
}

// serveStubAgent stands up a happy-path guest agent behind Firecracker's
// hybrid-vsock handshake on sock, answering the refresh routes the fork
// path calls so clone-safety succeeds without a real guest.
func serveStubAgent(t *testing.T, sock string) {
	t.Helper()
	raw, err := net.Listen("unix", sock)
	if err != nil {
		t.Errorf("listen unix: %v", err)
		return
	}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
	mux.HandleFunc("POST /identity/refresh", ok)
	mux.HandleFunc("POST /network/refresh", ok)
	srv := &http.Server{Handler: mux, ReadTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() { _ = srv.Serve(hybridVsockListener{raw: raw}); close(done) }()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
}

// hybridVsockListener answers the "CONNECT <port>\n" → "OK 42\n" handshake
// Firecracker's hybrid vsock uses, then hands the stream to net/http.
type hybridVsockListener struct{ raw net.Listener }

func (l hybridVsockListener) Accept() (net.Conn, error) {
	conn, err := l.raw.Accept()
	if err != nil {
		return nil, err
	}
	one := make([]byte, 1)
	var line strings.Builder
	for !strings.HasSuffix(line.String(), "\n") {
		if _, err := conn.Read(one); err != nil {
			_ = conn.Close()
			return nil, err
		}
		line.Write(one)
	}
	if _, err := conn.Write([]byte("OK 42\n")); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (l hybridVsockListener) Close() error   { return l.raw.Close() }
func (l hybridVsockListener) Addr() net.Addr { return l.raw.Addr() }

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
		Runner:   &stubRunner{t: t},
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
	defer func() { _ = resp.Body.Close() }()
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
	var got api.SandboxResponse
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
	var got api.SandboxResponse
	decodeJSON(t, resp, &got)
	if got.VCPUs != 4 || got.MemoryMiB != 2048 {
		t.Errorf("got %+v", got)
	}
}

func TestCreateSandboxRejectsOversizedResources(t *testing.T) {
	ts, _ := newTestServer(t)

	cases := map[string]string{
		"vcpus":      `{"vcpus": 1000}`,
		"memory_mib": `{"memory_mib": 999999999}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewBufferString(body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
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
	var e api.ErrorResponse
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
	var e api.ErrorResponse
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
	var e api.ErrorResponse
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
	var created api.SandboxResponse
	decodeJSON(t, resp, &created)

	resp, err = http.Get(ts.URL + "/sandboxes/" + created.ID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got api.SandboxResponse
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
	var empty api.ListResponse
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
	var got api.ListResponse
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
	var created api.SandboxResponse
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
	var created api.SandboxResponse
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
	var created api.SandboxResponse
	decodeJSON(t, resp, &created)

	body := strings.NewReader(`{"cmd":["/bin/true"]}`)
	resp, err = http.Post(ts.URL+"/sandboxes/"+created.ID+"/exec", "application/json", body)
	if err != nil {
		t.Fatalf("POST /exec: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
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
	var out api.SandboxResponse
	decodeJSON(t, resp, &out)
	return out.ID
}

func createSnapshotViaHTTP(t *testing.T, ts *httptest.Server, sbxID string) api.SnapshotResponse {
	t.Helper()
	resp, err := http.Post(ts.URL+"/sandboxes/"+sbxID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("POST snapshot: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("snapshot status = %d, body = %s", resp.StatusCode, body)
	}
	var out api.SnapshotResponse
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
	var empty api.SnapshotListResponse
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
	var got api.SnapshotListResponse
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
	var got api.SnapshotResponse
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
	var got api.ForkResponse
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
	var got api.ForkResponse
	decodeJSON(t, resp, &got)
	if len(got.Sandboxes) != 1 {
		t.Errorf("len = %d, want 1", len(got.Sandboxes))
	}
}

func TestForkSnapshotRejectsBadCount(t *testing.T) {
	ts, _ := newTestServer(t)
	sbxID := createSandboxViaHTTP(t, ts)
	snap := createSnapshotViaHTTP(t, ts, sbxID)

	// Includes oversized counts: the upper bound (DefaultMaxForkCount) must
	// be rejected cleanly with a 400 rather than allocating proportional to
	// count and OOMing the daemon.
	for _, q := range []string{"0", "-1", "not-a-number", "65", "50000000"} {
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

// streamPastWriteTimeout serves a chunked body that streams for longer than
// the server's WriteTimeout, optionally clearing the write deadline the way
// handleExecSandbox does. It is the mechanism M7's fix relies on: the exec
// route must SetWriteDeadline(zero) so a long exec isn't truncated mid-stream.
func streamPastWriteTimeout(clearDeadline bool) http.HandlerFunc {
	const chunks = 6
	return func(w http.ResponseWriter, r *http.Request) {
		if clearDeadline {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(40 * time.Millisecond) // total ~240ms, well past WriteTimeout
		}
	}
}

// serveWith starts a real http.Server (not httptest's default, which has no
// WriteTimeout) with the given WriteTimeout and handler.
func serveWith(t *testing.T, writeTimeout time.Duration, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.Config.WriteTimeout = writeTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func TestExecClearsWriteDeadlineSoLongStreamsSurvive(t *testing.T) {
	const writeTimeout = 80 * time.Millisecond

	// Control: without clearing the deadline, a stream longer than
	// WriteTimeout is cut off — proving the test actually exercises the
	// timeout and isn't a no-op.
	t.Run("not cleared truncates", func(t *testing.T) {
		ts := serveWith(t, writeTimeout, streamPastWriteTimeout(false))
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err == nil && len(body) == 6 {
			t.Fatalf("stream completed (%d bytes) despite WriteTimeout — control is broken", len(body))
		}
	})

	// Fix: clearing the deadline (as handleExecSandbox does) lets the full
	// stream through even though it runs well past WriteTimeout.
	t.Run("cleared streams to completion", func(t *testing.T) {
		ts := serveWith(t, writeTimeout, streamPastWriteTimeout(true))
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v (stream truncated despite cleared deadline)", err)
		}
		if len(body) != 6 {
			t.Fatalf("got %d bytes, want 6 — stream did not complete", len(body))
		}
	})
}

// newBareServer builds a Server (real middleware chain) for tests that need
// to exercise a middleware directly rather than the wired routes.
func newBareServer(t *testing.T) *Server {
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
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return srv
}

// TestExecFlushReachesWireThroughMiddleware proves the flush-forwarding fix:
// handleExecSandbox flushes each frame via ResponseController, which walks the
// middleware's Unwrap chain to the real Flusher. Behind logRequests'
// loggingResponseWriter a direct w.(http.Flusher) assertion is nil (its
// embedded-interface method set doesn't promote Flush), so without the fix the
// bytes — headers included — stay buffered in net/http until the handler
// returns. The test holds the handler open and asserts the client receives the
// first frame *while the handler is still blocked*: only a real mid-handler
// flush makes http.Get + read complete before the handler is released.
func TestExecFlushReachesWireThroughMiddleware(t *testing.T) {
	srv := newBareServer(t)

	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)

	const hold = 5 * time.Second
	h := srv.logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Mirror flushOnWrite: write a frame, then flush via the controller.
		_, _ = w.Write([]byte("early"))
		_ = http.NewResponseController(w).Flush()
		// Hold the response open. Bounded so a failing run can't wedge the
		// handler and deadlock ts.Close during cleanup.
		select {
		case <-released:
		case <-time.After(hold):
		}
	}))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// The whole client exchange (headers + first frame) must complete while the
	// handler is still blocked. Without the flush, http.Get itself blocks on
	// headers until the handler returns (~hold), so this whole goroutine would
	// miss the deadline below.
	got := make(chan error, 1)
	go func() {
		resp, err := http.Get(ts.URL)
		if err != nil {
			got <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		buf := make([]byte, len("early"))
		if _, rerr := io.ReadFull(resp.Body, buf); rerr != nil {
			got <- rerr
			return
		}
		if string(buf) != "early" {
			got <- fmt.Errorf("first frame = %q, want %q", buf, "early")
			return
		}
		got <- nil
	}()

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("client exchange: %v", err)
		}
	case <-time.After(hold - 2*time.Second):
		t.Fatal("first frame never arrived mid-handler — flush did not reach the wire through the middleware")
	}
	release()
}

// TestSnapshotSurvivesWriteTimeoutOverHTTP proves the N2 fix: a snapshot
// whose server-side work outlasts the server WriteTimeout still delivers its
// ID to the client. The WriteTimeout is armed once at request start; before
// the fix, handleCreateSnapshot never cleared it, so the snapshot was written
// and registered but writeJSON failed on the long-passed deadline and the
// client learned no ID (orphan). handleForkSnapshot had the same hole (leak).
func TestSnapshotSurvivesWriteTimeoutOverHTTP(t *testing.T) {
	const writeTimeout = 100 * time.Millisecond
	// The snapshot's memory-file write is modelled by snapDelay; it straddles
	// the WriteTimeout so the response would be guillotined without the fix.
	const snapDelay = 350 * time.Millisecond

	// Control: mirror the OLD route — do the slow work, then writeJSON without
	// clearing the deadline — and confirm WriteTimeout truncates it, so the
	// success case below is exercising a real deadline, not a no-op.
	t.Run("control: unclearing route is truncated", func(t *testing.T) {
		ts := serveWith(t, writeTimeout, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(snapDelay)
			writeJSON(w, http.StatusCreated, api.SnapshotResponse{ID: "snap_control"})
		}))
		resp, err := http.Get(ts.URL)
		if err != nil {
			return // connection reset before headers — also a truncation
		}
		body, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr == nil && len(body) > 0 {
			t.Fatalf("control delivered a body (%q) past WriteTimeout — deadline not exercised", body)
		}
	})

	t.Run("snapshot route clears the deadline and returns the ID", func(t *testing.T) {
		workBase := t.TempDir()
		tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
		if err := os.WriteFile(tmpl, []byte("fake-template"), 0o640); err != nil {
			t.Fatalf("write template rootfs: %v", err)
		}
		mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
			Runner:   &stubRunner{t: t, snapDelay: snapDelay},
			WorkBase: workBase,
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
		})
		if err != nil {
			t.Fatalf("daemon.New: %v", err)
		}
		ts := serveWith(t, writeTimeout, srv.Handler())

		sbxID := createSandboxViaHTTP(t, ts)

		start := time.Now()
		resp, err := http.Post(ts.URL+"/sandboxes/"+sbxID+"/snapshot", "application/json", nil)
		if err != nil {
			t.Fatalf("POST snapshot: %v", err)
		}
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		var got api.SnapshotResponse
		decodeJSON(t, resp, &got)
		if !strings.HasPrefix(got.ID, "snap_") {
			t.Fatalf("snapshot id %q: client did not learn a valid ID", got.ID)
		}
		if elapsed := time.Since(start); elapsed < writeTimeout {
			t.Fatalf("snapshot returned in %v, under WriteTimeout %v — test did not straddle the deadline", elapsed, writeTimeout)
		}
	})
}

// TestForkSurvivesWriteTimeoutOverHTTP is the fork-route counterpart of the
// snapshot test above: a fork whose restore outlasts the WriteTimeout must
// still return its sandbox IDs, or the client believes the fork failed while
// the sandboxes are live and unreaped (leak).
func TestForkSurvivesWriteTimeoutOverHTTP(t *testing.T) {
	const writeTimeout = 100 * time.Millisecond
	const restoreDelay = 350 * time.Millisecond

	workBase := t.TempDir()
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("fake-template"), 0o640); err != nil {
		t.Fatalf("write template rootfs: %v", err)
	}
	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:   &stubRunner{t: t, restoreDelay: restoreDelay},
		WorkBase: workBase,
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
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := serveWith(t, writeTimeout, srv.Handler())

	sbxID := createSandboxViaHTTP(t, ts)
	snap := createSnapshotViaHTTP(t, ts, sbxID)

	start := time.Now()
	resp, err := http.Post(ts.URL+"/snapshots/"+snap.ID+"/fork", "application/json", nil)
	if err != nil {
		t.Fatalf("POST fork: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got api.ForkResponse
	decodeJSON(t, resp, &got)
	if len(got.Sandboxes) != 1 || !strings.HasPrefix(got.Sandboxes[0].ID, "sbx_") {
		t.Fatalf("fork response %+v: client did not learn a valid sandbox ID", got)
	}
	if elapsed := time.Since(start); elapsed < writeTimeout {
		t.Fatalf("fork returned in %v, under WriteTimeout %v — test did not straddle the deadline", elapsed, writeTimeout)
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
