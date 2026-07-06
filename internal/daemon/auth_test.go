package daemon

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/internal/tokenstore"
)

func serverWithAuth(t *testing.T, store *tokenstore.Store) *Server {
	t.Helper()
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	mgr, err := sandbox.NewManager(sandbox.ManagerConfig{
		Runner: &stubRunner{t: t}, WorkBase: t.TempDir(), Kernel: "/k", Rootfs: tmpl,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv, err := New(Config{
		Manager:    mgr,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		TokenStore: store,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return srv
}

func do(t *testing.T, srv *Server, method, path, auth string) int {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec.Code
}

func TestAuthDisabledWhenNoTokens(t *testing.T) {
	// An empty store means auth is off — the loopback default.
	store := tokenstore.Open(filepath.Join(t.TempDir(), "tokens.json"))
	srv := serverWithAuth(t, store)
	if code := do(t, srv, "GET", "/sandboxes", ""); code != 200 {
		t.Fatalf("empty store: GET /sandboxes = %d, want 200", code)
	}
}

func TestAuthEnforcedWhenTokensExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	raw, _, err := tokenstore.Add(path, "k")
	if err != nil {
		t.Fatal(err)
	}
	srv := serverWithAuth(t, tokenstore.Open(path))

	if code := do(t, srv, "GET", "/sandboxes", ""); code != 401 {
		t.Errorf("no token: %d, want 401", code)
	}
	if code := do(t, srv, "GET", "/sandboxes", "Bearer crucible_wrong"); code != 401 {
		t.Errorf("wrong token: %d, want 401", code)
	}
	if code := do(t, srv, "GET", "/sandboxes", "Bearer "+raw); code != 200 {
		t.Errorf("valid token: %d, want 200", code)
	}
	// /healthz is exempt even without a token.
	if code := do(t, srv, "GET", "/healthz", ""); code != 200 {
		t.Errorf("healthz without token: %d, want 200 (exempt)", code)
	}
}
