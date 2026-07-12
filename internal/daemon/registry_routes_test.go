package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/internal/registryauth"
	"github.com/gnana997/crucible/internal/tokenstore"
	"github.com/gnana997/crucible/sdk/api"
)

// registryServer builds a Server with a registry credential store (and an
// optional token store for the policy test).
func registryServer(t *testing.T, tokens *tokenstore.Store) (*Server, *registryauth.Store) {
	t.Helper()
	reg := registryauth.Open(filepath.Join(t.TempDir(), "registry.json"))
	srv, err := New(Config{
		Manager:       stubSandboxManager(t),
		Addr:          "127.0.0.1:0",
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		TokenStore:    tokens,
		RegistryStore: reg,
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return srv, reg
}

// TestRegistryCredentialsCRUD: login stores the cred; list shows host+username
// but never the secret; logout removes it; unknown logout is 404; a login with
// no secret is 400.
func TestRegistryCredentialsCRUD(t *testing.T) {
	srv, reg := registryServer(t, nil) // keyless loopback → no auth needed

	if code, b := call(t, srv, "POST", "/registry/credentials", "",
		`{"host":"ghcr.io","username":"alice","secret":"tok-xyz"}`); code != 201 {
		t.Fatalf("login = %d (%s), want 201", code, b)
	}
	if c, ok := reg.Lookup("ghcr.io"); !ok || c.Secret != "tok-xyz" {
		t.Fatalf("store lookup = %+v ok=%v", c, ok)
	}

	code, b := call(t, srv, "GET", "/registry/credentials", "", "")
	if code != 200 {
		t.Fatalf("list = %d (%s)", code, b)
	}
	if strings.Contains(b, "tok-xyz") {
		t.Errorf("list leaked the secret: %s", b)
	}
	var lr api.RegistryCredentialListResponse
	if err := json.Unmarshal([]byte(b), &lr); err != nil {
		t.Fatal(err)
	}
	if len(lr.Registries) != 1 || lr.Registries[0].Host != "ghcr.io" || lr.Registries[0].Username != "alice" {
		t.Fatalf("list body = %+v", lr.Registries)
	}

	if code, _ := call(t, srv, "DELETE", "/registry/credentials/ghcr.io", "", ""); code != 204 {
		t.Errorf("logout = %d, want 204", code)
	}
	if _, ok := reg.Lookup("ghcr.io"); ok {
		t.Error("cred present after logout")
	}
	if code, _ := call(t, srv, "DELETE", "/registry/credentials/ghcr.io", "", ""); code != 404 {
		t.Errorf("logout unknown = %d, want 404", code)
	}
	if code, _ := call(t, srv, "POST", "/registry/credentials", "", `{"host":"ghcr.io"}`); code != 400 {
		t.Errorf("login without secret = %d, want 400", code)
	}
}

// TestRegistryRoutesDisabled: no store → 501.
func TestRegistryRoutesDisabled(t *testing.T) {
	srv, err := New(Config{
		Manager: stubSandboxManager(t),
		Addr:    "127.0.0.1:0",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, srv, "GET", "/registry/credentials", "", ""); code != 501 {
		t.Errorf("list with no store = %d, want 501", code)
	}
	if code, _ := call(t, srv, "POST", "/registry/credentials", "", `{"host":"x","secret":"y"}`); code != 501 {
		t.Errorf("login with no store = %d, want 501", code)
	}
}

// TestRegistryPolicyGate: POST/DELETE need the `registry` op; GET is read-gated.
func TestRegistryPolicyGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	rawNo, _, err := tokenstore.Add(path, tokenstore.AddOptions{
		Name: "noreg", Policy: &policy.Policy{Operations: []policy.Operation{policy.OpRead}}})
	if err != nil {
		t.Fatal(err)
	}
	rawYes, _, err := tokenstore.Add(path, tokenstore.AddOptions{
		Name: "reg", Policy: &policy.Policy{Operations: []policy.Operation{policy.OpRead, policy.OpRegistry}}})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := registryServer(t, tokenstore.Open(path))

	body := `{"host":"ghcr.io","username":"a","secret":"s"}`
	if code, _ := call(t, srv, "POST", "/registry/credentials", rawNo, body); code != 403 {
		t.Errorf("login without registry op = %d, want 403", code)
	}
	if code, _ := call(t, srv, "GET", "/registry/credentials", rawNo, ""); code != 200 {
		t.Errorf("list with read op = %d, want 200", code)
	}
	if code, b := call(t, srv, "POST", "/registry/credentials", rawYes, body); code != 201 {
		t.Errorf("login with registry op = %d (%s), want 201", code, b)
	}
	if code, _ := call(t, srv, "DELETE", "/registry/credentials/ghcr.io", rawNo, ""); code != 403 {
		t.Errorf("logout without registry op = %d, want 403", code)
	}
}
