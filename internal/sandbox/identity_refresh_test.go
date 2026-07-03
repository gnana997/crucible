package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentapi"
	"github.com/gnana997/crucible/internal/agentwire"
)

// hybridStubListener speaks just enough of Firecracker's hybrid-vsock
// handshake (read "CONNECT <port>\n", answer "OK 42\n") for agentapi's
// Client to reach an http.Server in these tests. The full-featured
// mock lives in agentapi's own tests; this stub only needs the happy
// handshake.
type hybridStubListener struct {
	raw net.Listener
}

func (l hybridStubListener) Accept() (net.Conn, error) {
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

func (l hybridStubListener) Close() error   { return l.raw.Close() }
func (l hybridStubListener) Addr() net.Addr { return l.raw.Addr() }

// serveHybrid runs handler behind the stub handshake on sock. Safe to
// call from a spawned goroutine (delayed-agent tests): it reports
// failure via Errorf, not Fatalf.
func serveHybrid(t *testing.T, sock string, handler http.Handler) {
	t.Helper()
	raw, err := net.Listen("unix", sock)
	if err != nil {
		t.Errorf("listen unix: %v", err)
		return
	}
	srv := &http.Server{Handler: handler, ReadTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() { _ = srv.Serve(hybridStubListener{raw: raw}); close(done) }()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
}

// identityRecorder collects the requests a stub agent receives.
type identityRecorder struct {
	mu   sync.Mutex
	reqs []agentwire.IdentityRefreshRequest
}

func newIdentityRecorder() *identityRecorder { return &identityRecorder{} }

func (rec *identityRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /identity/refresh", func(w http.ResponseWriter, r *http.Request) {
		var req agentwire.IdentityRefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, req)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func (rec *identityRecorder) requests() []agentwire.IdentityRefreshRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]agentwire.IdentityRefreshRequest(nil), rec.reqs...)
}

func TestRefreshIdentityRetriesUntilAgentReady(t *testing.T) {
	// The refresh must not depend on WaitForAgent: when the agent is
	// still waking (no listener yet), refreshIdentity retries inside
	// its window instead of failing on the first dial error.
	sock := filepath.Join(t.TempDir(), "fc.sock")
	rec := newIdentityRecorder()
	go func() {
		time.Sleep(500 * time.Millisecond)
		serveHybrid(t, sock, rec.handler())
	}()

	m := &Manager{}
	c := agentapi.NewClient(sock, agentwire.AgentVSockPort)
	if err := m.refreshIdentity(context.Background(), c, "sb-late"); err != nil {
		t.Fatalf("refreshIdentity: %v", err)
	}
	reqs := rec.requests()
	if len(reqs) != 1 {
		t.Fatalf("agent saw %d refreshes, want 1", len(reqs))
	}
	if len(reqs[0].Seed) != identitySeedSize {
		t.Errorf("seed length = %d, want %d", len(reqs[0].Seed), identitySeedSize)
	}
	if reqs[0].SandboxID != "sb-late" {
		t.Errorf("sandbox_id = %q, want sb-late", reqs[0].SandboxID)
	}
}

func TestRefreshIdentitySeedsUniquePerFork(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fc.sock")
	rec := newIdentityRecorder()
	serveHybrid(t, sock, rec.handler())

	m := &Manager{}
	c := agentapi.NewClient(sock, agentwire.AgentVSockPort)
	if err := m.refreshIdentity(context.Background(), c, "sb-a"); err != nil {
		t.Fatalf("refreshIdentity sb-a: %v", err)
	}
	if err := m.refreshIdentity(context.Background(), c, "sb-b"); err != nil {
		t.Fatalf("refreshIdentity sb-b: %v", err)
	}
	reqs := rec.requests()
	if len(reqs) != 2 {
		t.Fatalf("agent saw %d refreshes, want 2", len(reqs))
	}
	if string(reqs[0].Seed) == string(reqs[1].Seed) {
		t.Error("two forks received the same seed — per-fork uniqueness broken")
	}
}

func TestRefreshIdentityStaleRootfsFailsFast(t *testing.T) {
	// An old agent has no /identity/refresh route → 404. That's not
	// a transient condition, so refreshIdentity must abort with the
	// sentinel immediately instead of burning its full retry window.
	sock := filepath.Join(t.TempDir(), "fc.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	serveHybrid(t, sock, mux)

	m := &Manager{}
	c := agentapi.NewClient(sock, agentwire.AgentVSockPort)
	start := time.Now()
	err := m.refreshIdentity(context.Background(), c, "sb-stale")
	if !errors.Is(err, agentapi.ErrIdentityRefreshUnsupported) {
		t.Fatalf("err = %v, want ErrIdentityRefreshUnsupported", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v; a stale rootfs must fail fast, not retry out the window", elapsed)
	}
}
