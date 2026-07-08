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
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/sandbox"
)

// newImageCreateServer wires a real Manager (service stub runner, so
// create passes the agent gate) plus a seedable fake image store, and
// exposes the runner so tests can inspect the boot spec.
func newImageCreateServer(t *testing.T, store ImageStore) (*httptest.Server, *serviceStubRunner) {
	t.Helper()
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("fake-template"), 0o640); err != nil {
		t.Fatalf("write template rootfs: %v", err)
	}
	runner := &serviceStubRunner{stubRunner: stubRunner{t: t}, agent: &stubServiceAgent{}}
	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:   runner,
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
		Images:  store,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	return ts, runner
}

func TestCreateFromOCIImageBootsWithInitAgent(t *testing.T) {
	// A converted image on disk (any readable file — the stub runner
	// doesn't boot it) resolvable by ref.
	imgRootfs := filepath.Join(t.TempDir(), "converted.ext4")
	if err := os.WriteFile(imgRootfs, []byte("converted-image"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := newFakeImageStore()
	store.seed("sha256:"+strings.Repeat("c", 64), imgRootfs)

	ts, runner := newImageCreateServer(t, store)

	body := bytes.NewBufferString(`{"image":{"oci":"sha256:` + strings.Repeat("c", 64) + `"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d: %s", resp.StatusCode, b)
	}
	var sb api.SandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The runner booted with init=/crucible/crucible-agent so the guest
	// runs the agent as PID 1.
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) == 0 {
		t.Fatal("runner.Start was never called")
	}
	bootArgs := runner.calls[len(runner.calls)-1].BootArgs
	if !strings.Contains(bootArgs, "init=/crucible/crucible-agent") {
		t.Errorf("boot args = %q, want init=/crucible/crucible-agent", bootArgs)
	}
}

func TestCreateFromOCIImageUnknownRef404(t *testing.T) {
	store := newFakeImageStore()
	ts, _ := newImageCreateServer(t, store)
	body := bytes.NewBufferString(`{"image":{"oci":"sha256:missing"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create with unknown image = %d, want 404", resp.StatusCode)
	}
}

func TestCreateFromOCIImageRejectsNetwork(t *testing.T) {
	imgRootfs := filepath.Join(t.TempDir(), "converted.ext4")
	if err := os.WriteFile(imgRootfs, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := newFakeImageStore()
	store.seed("sha256:net", imgRootfs)
	ts, _ := newImageCreateServer(t, store)

	body := bytes.NewBufferString(`{"image":{"oci":"sha256:net"},"network":{"enabled":true,"allowlist":["pypi.org"]}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("image+network = %d, want 400", resp.StatusCode)
	}
}
