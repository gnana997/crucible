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
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/api"
	"github.com/gnana997/crucible/internal/oci"
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
	// doesn't boot it) resolvable by ref, with a runtime contract.
	imgRootfs := filepath.Join(t.TempDir(), "converted.ext4")
	if err := os.WriteFile(imgRootfs, []byte("converted-image"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := newFakeImageStore()
	rec := store.seed("sha256:"+strings.Repeat("c", 64), imgRootfs)
	rec.RunConfig = &oci.RunConfig{
		Entrypoint: []string{"/docker-entrypoint.sh"},
		Cmd:        []string{"nginx"},
		Env:        []string{"NGINX=1"},
		User:       "nginx",
		StopSignal: "SIGQUIT",
	}

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
	bootArgs := ""
	if len(runner.calls) > 0 {
		bootArgs = runner.calls[len(runner.calls)-1].BootArgs
	}
	runner.mu.Unlock()
	if !strings.Contains(bootArgs, "init=/crucible/crucible-agent") {
		t.Errorf("boot args = %q, want init=/crucible/crucible-agent", bootArgs)
	}

	// The image's entrypoint was pushed to the guest agent as the
	// service spec (docker-run fidelity): image Entrypoint+Cmd, user,
	// stop signal, exact env.
	runner.agent.mu.Lock()
	spec := runner.agent.spec
	runner.agent.mu.Unlock()
	if spec == nil {
		t.Fatal("no service spec pushed to the agent")
	}
	if len(spec.Cmd) != 2 || spec.Cmd[0] != "/docker-entrypoint.sh" || spec.Cmd[1] != "nginx" {
		t.Errorf("service cmd = %v, want the image entrypoint+cmd", spec.Cmd)
	}
	if spec.User != "nginx" || spec.StopSignal != "SIGQUIT" || !spec.EnvExact {
		t.Errorf("service fidelity fields: user=%q stop=%q exact=%v", spec.User, spec.StopSignal, spec.EnvExact)
	}
	if spec.Env["NGINX"] != "1" {
		t.Errorf("service env = %v, want NGINX=1", spec.Env)
	}
}

func TestCreateFromOCIImagePullNeverMiss404(t *testing.T) {
	// With --pull never, a store miss is a 404 and the daemon never
	// touches the network (Pull is not called).
	store := newFakeImageStore()
	ts, _ := newImageCreateServer(t, store)
	body := bytes.NewBufferString(`{"image":{"oci":"sha256:missing"},"pull":"never"}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create --pull never on a miss = %d, want 404", resp.StatusCode)
	}
	if store.lastPull != "" {
		t.Errorf("pull=never still pulled %q", store.lastPull)
	}
}

func TestCreateFromOCIImagePullsOnMiss(t *testing.T) {
	// The default (missing) policy acquires an uncached image: a bare
	// `create --image nginx:latest` pulls + converts, then boots. This is
	// the one-command headline.
	imgRootfs := filepath.Join(t.TempDir(), "pulled.ext4")
	if err := os.WriteFile(imgRootfs, []byte("pulled-image"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := newFakeImageStore()
	store.pullRootfs = imgRootfs // pulled record carries a usable rootfs
	ts, _ := newImageCreateServer(t, store)

	body := bytes.NewBufferString(`{"image":{"oci":"nginx:latest"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create on a miss = %d, want 201 (should pull): %s", resp.StatusCode, b)
	}
	if store.lastPull != "nginx:latest" {
		t.Errorf("store saw pull %q, want nginx:latest", store.lastPull)
	}
}

func TestCreateFromOCIImageAlwaysRepulls(t *testing.T) {
	// --pull always re-acquires even when the ref is already converted.
	imgRootfs := filepath.Join(t.TempDir(), "img.ext4")
	if err := os.WriteFile(imgRootfs, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := newFakeImageStore()
	store.seed("nginx:latest", imgRootfs) // a store hit exists…
	store.pullRootfs = imgRootfs
	ts, _ := newImageCreateServer(t, store)

	body := bytes.NewBufferString(`{"image":{"oci":"nginx:latest"},"pull":"always"}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create --pull always = %d, want 201: %s", resp.StatusCode, b)
	}
	if store.lastPull != "nginx:latest" {
		t.Errorf("--pull always did not re-pull (lastPull=%q)", store.lastPull)
	}
}

func TestCreateFromOCIImagePullFailure502(t *testing.T) {
	// A pull/convert failure (unknown ref, registry down) surfaces as a
	// gateway error, not a crash or a misleading 404.
	store := newFakeImageStore()
	store.pullErr = errors.New("registry: manifest unknown")
	ts, _ := newImageCreateServer(t, store)
	body := bytes.NewBufferString(`{"image":{"oci":"nope:latest"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("create with pull failure = %d, want 502", resp.StatusCode)
	}
}

func TestCreateInvalidPullPolicy400(t *testing.T) {
	store := newFakeImageStore()
	ts, _ := newImageCreateServer(t, store)
	body := bytes.NewBufferString(`{"image":{"oci":"nginx:latest"},"pull":"sometimes"}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid pull policy = %d, want 400", resp.StatusCode)
	}
}

func TestCreateFromOCIImageWithNetworkIsAccepted(t *testing.T) {
	// m2 lifted the old "no networking for OCI images" rejection: an
	// image sandbox with a valid allowlist is now accepted at the
	// validation layer (the daemon here has no network provisioner, so
	// it fails later at provisioning — but with 500, not the old 400).
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
	// Not a 400 (no longer rejected as unsupported). The test manager
	// has no network provisioner, so this daemon config can't fulfill
	// it — a 500, the same as any networked create without networking.
	if resp.StatusCode == http.StatusBadRequest {
		t.Fatalf("image+network rejected with 400; m2 should accept it")
	}
}
