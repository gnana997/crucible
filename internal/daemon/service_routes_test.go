package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/runner"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// stubServiceAgent is an in-memory stand-in for the guest agent's
// supervisor API, mounted behind the hybrid-vsock handshake. It records
// calls so tests can assert the daemon proxied faithfully.
type stubServiceAgent struct {
	mu    sync.Mutex
	spec  *wire.ServiceSpec
	state string
	calls []string
}

func (a *stubServiceAgent) status() wire.ServiceStatus {
	st := wire.ServiceStatus{State: a.state, Spec: a.spec}
	if a.state == wire.ServiceStateRunning {
		st.Pid = 4242
	}
	return st
}

func (a *stubServiceAgent) register(mux *http.ServeMux) {
	writeStatus := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.status())
	}
	mux.HandleFunc("PUT /service", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var spec wire.ServiceSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.spec, a.state = &spec, wire.ServiceStateStopped
		a.calls = append(a.calls, "configure")
		writeStatus(w)
	})
	action := func(name, nextState string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.spec == nil {
				http.Error(w, "no service configured", http.StatusConflict)
				return
			}
			a.state = nextState
			a.calls = append(a.calls, name)
			writeStatus(w)
		}
	}
	mux.HandleFunc("POST /service/start", action("start", wire.ServiceStateRunning))
	mux.HandleFunc("POST /service/stop", action("stop", wire.ServiceStateStopped))
	mux.HandleFunc("POST /service/restart", action("restart", wire.ServiceStateRunning))
	mux.HandleFunc("GET /service/status", func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		writeStatus(w)
	})
	mux.HandleFunc("GET /service/logs", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.spec == nil {
			http.Error(w, "no service configured", http.StatusConflict)
			return
		}
		a.calls = append(a.calls, "logs?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wire.ServiceLogsResponse{
			Records: []wire.ServiceLogRecord{
				{Seq: 0, Stream: wire.ServiceLogStdout, Data: []byte("hi\n")},
			},
			NextSeq: 1,
		})
	})
}

func (a *stubServiceAgent) callNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// serviceStubRunner serves the stubServiceAgent behind every *Start*ed
// sandbox's vsock (the base stubRunner only does that for Restore).
type serviceStubRunner struct {
	stubRunner
	agent *stubServiceAgent
}

func (r *serviceStubRunner) Start(ctx context.Context, spec runner.Spec) (runner.Handle, error) {
	h, err := r.stubRunner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(spec.Workdir, "svc.sock")
	serveStubServiceAgent(r.t, sock, r.agent)
	h.(*stubHandle).vsock = sock
	return h, nil
}

func serveStubServiceAgent(t *testing.T, sock string, agent *stubServiceAgent) {
	t.Helper()
	raw, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	agent.register(mux)
	registerEchoExec(t, mux)
	srv := &http.Server{Handler: mux, ReadTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() { _ = srv.Serve(hybridVsockListener{raw: raw}); close(done) }()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
}

// newServiceTestServer is newTestServer with a vsock-served stub agent
// behind every created sandbox.
func newServiceTestServer(t *testing.T) (*httptest.Server, *stubServiceAgent) {
	t.Helper()
	workBase := t.TempDir()
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("fake-template"), 0o640); err != nil {
		t.Fatalf("write template rootfs: %v", err)
	}
	agent := &stubServiceAgent{}
	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner:   &serviceStubRunner{stubRunner: stubRunner{t: t}, agent: agent},
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
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	return ts, agent
}

func createServiceTestSandbox(t *testing.T, ts *httptest.Server, body string) api.SandboxResponse {
	t.Helper()
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST /sandboxes: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d: %s", resp.StatusCode, b)
	}
	var sb api.SandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
		t.Fatalf("decode sandbox: %v", err)
	}
	return sb
}

func serviceReq(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestServiceRoutesLifecycle(t *testing.T) {
	ts, agent := newServiceTestServer(t)
	sb := createServiceTestSandbox(t, ts, `{}`)
	base := ts.URL + "/sandboxes/" + sb.ID + "/service"

	// Configure.
	resp := serviceReq(t, http.MethodPut, base, `{"cmd":["/bin/app","--serve"]}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT service = %d: %s", resp.StatusCode, b)
	}
	var st wire.ServiceStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.State != wire.ServiceStateStopped || st.Spec == nil {
		t.Fatalf("status after configure = %+v", st)
	}

	// Start → Status → Logs → Stop.
	if resp := serviceReq(t, http.MethodPost, base+"/start", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	resp = serviceReq(t, http.MethodGet, base, "")
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.State != wire.ServiceStateRunning {
		t.Fatalf("status = %+v (err %v), want running", st, err)
	}
	resp = serviceReq(t, http.MethodGet, base+"/logs?from_seq=0&max_bytes=512", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs = %d", resp.StatusCode)
	}
	var logs wire.ServiceLogsResponse
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil || len(logs.Records) != 1 {
		t.Fatalf("logs = %+v (err %v)", logs, err)
	}
	if resp := serviceReq(t, http.MethodPost, base+"/stop", `{"grace_s":3}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("stop = %d", resp.StatusCode)
	}

	want := []string{"configure", "start", "logs?from_seq=0&max_bytes=512", "stop"}
	got := agent.callNames()
	if len(got) != len(want) {
		t.Fatalf("agent calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("agent calls = %v, want %v", got, want)
		}
	}
}

func TestServiceRoutesValidation(t *testing.T) {
	ts, _ := newServiceTestServer(t)
	sb := createServiceTestSandbox(t, ts, `{}`)
	base := ts.URL + "/sandboxes/" + sb.ID + "/service"

	cases := []struct {
		name, method, url, body string
		want                    int
	}{
		{"bad json", http.MethodPut, base, `{nope`, http.StatusBadRequest},
		{"empty cmd", http.MethodPut, base, `{"cmd":[]}`, http.StatusBadRequest},
		{"bad signal", http.MethodPut, base, `{"cmd":["/x"],"stop_signal":"SIGNOPE"}`, http.StatusBadRequest},
		{"bad policy", http.MethodPut, base, `{"cmd":["/x"],"restart":{"policy":"sometimes"}}`, http.StatusBadRequest},
		{"negative grace", http.MethodPost, base + "/stop", `{"grace_s":-1}`, http.StatusBadRequest},
		{"start before configure", http.MethodPost, base + "/start", "", http.StatusConflict},
		{"bad id", http.MethodGet, ts.URL + "/sandboxes/not-an-id/service", "", http.StatusBadRequest},
		{"unknown sandbox", http.MethodGet, ts.URL + "/sandboxes/sbx_0000000000000/service", "", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := serviceReq(t, tc.method, tc.url, tc.body)
			if resp.StatusCode != tc.want {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("%s %s = %d, want %d (%s)", tc.method, tc.url, resp.StatusCode, tc.want, b)
			}
		})
	}
}

func TestCreateSandboxWithService(t *testing.T) {
	ts, agent := newServiceTestServer(t)
	sb := createServiceTestSandbox(t, ts, `{"service":{"cmd":["/bin/app"],"restart":{"policy":"always"}}}`)
	if sb.ID == "" {
		t.Fatal("no sandbox id")
	}
	got := agent.callNames()
	if len(got) != 2 || got[0] != "configure" || got[1] != "start" {
		t.Fatalf("agent calls = %v, want [configure start]", got)
	}
	// The service proxies work against the created sandbox.
	resp := serviceReq(t, http.MethodGet, ts.URL+"/sandboxes/"+sb.ID+"/service", "")
	var st wire.ServiceStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.State != wire.ServiceStateRunning {
		t.Fatalf("status = %+v (err %v), want running", st, err)
	}
}

func TestCreateSandboxWithBadServiceIs400(t *testing.T) {
	ts, agent := newServiceTestServer(t)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json",
		bytes.NewBufferString(`{"service":{"cmd":[]}}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with bad service = %d, want 400", resp.StatusCode)
	}
	if calls := agent.callNames(); len(calls) != 0 {
		t.Fatalf("agent was called (%v) despite validation failure", calls)
	}
}
