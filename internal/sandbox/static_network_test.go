package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/runner"
)

func TestStaticNetConfigFromHandle(t *testing.T) {
	req := staticNetConfig("sbx_abc", &NetworkHandle{
		GuestIP:    "10.20.0.14",
		Gateway:    "10.20.0.13",
		PrefixBits: 30,
		DNSServer:  "10.20.255.254",
	})
	if req.Address != "10.20.0.14" || req.PrefixLen != 30 || req.Gateway != "10.20.0.13" {
		t.Errorf("address/prefix/gateway wrong: %+v", req)
	}
	if req.Hostname != "sbx_abc" {
		t.Errorf("hostname = %q, want sbx_abc", req.Hostname)
	}
	if len(req.DNS) != 1 || req.DNS[0] != "10.20.255.254" {
		t.Errorf("dns = %v", req.DNS)
	}
}

// netConfigRecorder records the /network/configure requests a stub
// guest agent receives.
type netConfigRecorder struct {
	mu   sync.Mutex
	reqs []agentwire.NetworkConfigRequest
}

func (rec *netConfigRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /network/configure", func(w http.ResponseWriter, r *http.Request) {
		var req agentwire.NetworkConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, req)
		rec.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func (rec *netConfigRecorder) requests() []agentwire.NetworkConfigRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]agentwire.NetworkConfigRequest(nil), rec.reqs...)
}

// agentRunner serves the given handler behind every started sandbox's
// vsock so Manager.Create can reach a (stub) guest agent.
type agentRunner struct {
	t       *testing.T
	handler http.Handler
	mu      sync.Mutex
	calls   []runner.Spec
}

func (r *agentRunner) Start(_ context.Context, spec runner.Spec) (runner.Handle, error) {
	r.mu.Lock()
	r.calls = append(r.calls, spec)
	r.mu.Unlock()
	_ = os.MkdirAll(spec.Workdir, 0o755)
	h := newStubHandle(spec.Workdir)
	sock := filepath.Join(spec.Workdir, "a.sock")
	serveHybrid(r.t, sock, r.handler)
	h.vsock = sock
	return h, nil
}

func (r *agentRunner) Restore(context.Context, runner.RestoreSpec) (runner.Handle, error) {
	return nil, context.Canceled
}

// staticNetProvisioner returns a handle carrying the fields the static
// network push needs.
type staticNetProvisioner struct{}

func (staticNetProvisioner) Setup(context.Context, NetworkSetupRequest) (*NetworkHandle, error) {
	return &NetworkHandle{
		NetnsPath:  "/var/run/netns/crucible-stub",
		TapName:    "tap0",
		GuestMAC:   "02:00:00:00:00:01",
		GuestIP:    "10.20.0.6",
		Gateway:    "10.20.0.5",
		PrefixBits: 30,
		DNSServer:  "10.20.255.254",
		Allowlist:  stubAllowlist{},
	}, nil
}

func (staticNetProvisioner) Teardown(context.Context, *NetworkHandle) error { return nil }

func TestCreateStaticNetworkPushesConfig(t *testing.T) {
	rec := &netConfigRecorder{}
	template := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(template, []byte("fake"), 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(ManagerConfig{
		Runner:       &agentRunner{t: t, handler: rec.handler()},
		WorkBase:     t.TempDir(),
		Kernel:       "/fake/vmlinux",
		Rootfs:       template,
		WaitForAgent: true,
		Network:      staticNetProvisioner{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Shutdown(context.Background()) })

	sb, err := m.Create(context.Background(), CreateConfig{
		StaticNetwork: true,
		Network:       &NetworkConfig{Allowlist: stubAllowlist{}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	reqs := rec.requests()
	if len(reqs) != 1 {
		t.Fatalf("agent got %d configure calls, want 1", len(reqs))
	}
	got := reqs[0]
	if got.Address != "10.20.0.6" || got.PrefixLen != 30 || got.Gateway != "10.20.0.5" {
		t.Errorf("pushed config = %+v", got)
	}
	if got.Hostname != sb.ID {
		t.Errorf("hostname = %q, want sandbox id %q", got.Hostname, sb.ID)
	}
	if len(got.DNS) != 1 || got.DNS[0] != "10.20.255.254" {
		t.Errorf("dns = %v", got.DNS)
	}
}

func TestCreateNonStaticNetworkNoPush(t *testing.T) {
	rec := &netConfigRecorder{}
	template := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(template, []byte("fake"), 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(ManagerConfig{
		Runner:       &agentRunner{t: t, handler: rec.handler()},
		WorkBase:     t.TempDir(),
		Kernel:       "/fake/vmlinux",
		Rootfs:       template,
		WaitForAgent: true,
		Network:      staticNetProvisioner{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Shutdown(context.Background()) })

	// A profile sandbox (StaticNetwork false) with network must NOT get a
	// static push — it DHCPs.
	if _, err := m.Create(context.Background(), CreateConfig{
		Network: &NetworkConfig{Allowlist: stubAllowlist{}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n := len(rec.requests()); n != 0 {
		t.Errorf("profile sandbox got %d static pushes, want 0", n)
	}
}
