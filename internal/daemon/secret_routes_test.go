package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/internal/secretstore"
	"github.com/gnana997/crucible/internal/tokenstore"
)

func secretServer(t *testing.T, tokens *tokenstore.Store) *Server {
	t.Helper()
	key, _ := secretstore.GenerateKey()
	st, err := secretstore.Open(filepath.Join(t.TempDir(), "secrets.db"), key)
	if err != nil {
		t.Fatalf("secretstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := New(Config{
		Manager:     stubSandboxManager(t),
		Addr:        "127.0.0.1:0",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		TokenStore:  tokens,
		SecretStore: st,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return srv
}

// The /secrets API is write-only: PUT stores a bundle; GET yields only names and
// key names; a value is NEVER returned anywhere.
func TestSecretRoutesWriteOnly(t *testing.T) {
	srv := secretServer(t, nil) // keyless loopback → no auth

	const secretVal = "super-sekret-value"
	if code, b := call(t, srv, "PUT", "/secrets/web-env", "",
		`{"data":{"DATABASE_URL":"`+secretVal+`","REDIS_URL":"redis://x"}}`); code != 204 {
		t.Fatalf("PUT = %d (%s), want 204", code, b)
	}

	// List: names only, no values.
	code, b := call(t, srv, "GET", "/secrets", "", "")
	if code != 200 || !strings.Contains(b, "web-env") {
		t.Fatalf("GET /secrets = %d (%s), want 200 with the name", code, b)
	}
	if strings.Contains(b, secretVal) {
		t.Fatal("GET /secrets leaked a value")
	}

	// Bundle keys: key names only, no values.
	code, b = call(t, srv, "GET", "/secrets/web-env", "", "")
	if code != 200 || !strings.Contains(b, "DATABASE_URL") || !strings.Contains(b, "REDIS_URL") {
		t.Fatalf("GET /secrets/web-env = %d (%s), want 200 with key names", code, b)
	}
	if strings.Contains(b, secretVal) {
		t.Fatal("GET /secrets/{name} leaked a value")
	}

	// Merge updates a key without touching others.
	if code, b := call(t, srv, "PUT", "/secrets/web-env", "",
		`{"merge":true,"data":{"API_KEY":"k"}}`); code != 204 {
		t.Fatalf("PUT merge = %d (%s), want 204", code, b)
	}
	if _, b := call(t, srv, "GET", "/secrets/web-env", "", ""); !strings.Contains(b, "API_KEY") || !strings.Contains(b, "DATABASE_URL") {
		t.Fatalf("after merge keys = %s, want API_KEY + DATABASE_URL", b)
	}

	// Delete → gone.
	if code, _ := call(t, srv, "DELETE", "/secrets/web-env", "", ""); code != 204 {
		t.Fatalf("DELETE = %d, want 204", code)
	}
	if code, _ := call(t, srv, "GET", "/secrets/web-env", "", ""); code != 404 {
		t.Fatalf("GET after delete = %d, want 404", code)
	}
}

// With no master key configured, the /secrets routes answer 501.
func TestSecretRoutesDisabled(t *testing.T) {
	srv, err := New(Config{
		Manager: stubSandboxManager(t),
		Addr:    "127.0.0.1:0",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, srv, "GET", "/secrets", "", ""); code != 501 {
		t.Fatalf("GET /secrets with no store = %d, want 501", code)
	}
}

// The secret routes are gated by the default-deny `secret` op: a read-only token
// can't touch them; a secret-scoped token can.
func TestSecretRoutesScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	readTok, _, err := tokenstore.Add(path, tokenstore.AddOptions{
		Name: "ro", Policy: &policy.Policy{Operations: []policy.Operation{policy.OpRead}}})
	if err != nil {
		t.Fatal(err)
	}
	secTok, _, err := tokenstore.Add(path, tokenstore.AddOptions{
		Name: "sec", Policy: &policy.Policy{Operations: []policy.Operation{policy.OpSecret}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := secretServer(t, tokenstore.Open(path))

	if code, _ := call(t, srv, "GET", "/secrets", readTok, ""); code != 403 {
		t.Fatalf("read-only token GET /secrets = %d, want 403", code)
	}
	if code, _ := call(t, srv, "GET", "/secrets", secTok, ""); code != 200 {
		t.Fatalf("secret token GET /secrets = %d, want 200", code)
	}
}
