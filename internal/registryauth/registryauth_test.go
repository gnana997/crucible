package registryauth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
)

func TestCanonicalHost(t *testing.T) {
	cases := map[string]string{
		"docker.io":             "index.docker.io",
		"index.docker.io":       "index.docker.io",
		"registry-1.docker.io":  "index.docker.io",
		"":                      "index.docker.io",
		"ghcr.io":               "ghcr.io",
		"GHCR.IO":               "ghcr.io",
		"https://ghcr.io/":      "ghcr.io",
		"http://localhost:5000": "localhost:5000",
	}
	for in, want := range cases {
		if got := canonicalHost(in); got != want {
			t.Errorf("canonicalHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStoreUpsertLookupDeleteList(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "registry.json"))
	if err := s.Upsert("ghcr.io", "alice", "tok1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert("docker.io", "bob", "tok2"); err != nil {
		t.Fatal(err)
	}

	// A docker.io alias resolves to the same entry (canonicalization).
	if c, ok := s.Lookup("registry-1.docker.io"); !ok || c.Username != "bob" || c.Secret != "tok2" {
		t.Fatalf("lookup docker alias = %+v ok=%v", c, ok)
	}

	// List never exposes the secret.
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("want 2 creds, got %d", len(list))
	}
	for _, c := range list {
		if c.Secret != "" {
			t.Errorf("List leaked a secret for %s", c.Host)
		}
	}

	// Upsert replaces in place.
	if err := s.Upsert("ghcr.io", "alice2", "tok1b"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Lookup("ghcr.io"); c.Username != "alice2" || c.Secret != "tok1b" {
		t.Errorf("upsert did not replace: %+v", c)
	}
	if len(s.List()) != 2 {
		t.Errorf("replace changed the count: %d", len(s.List()))
	}

	// Delete reports found/not-found.
	if ok, _ := s.Delete("ghcr.io"); !ok {
		t.Error("delete ghcr.io returned not-found")
	}
	if _, ok := s.Lookup("ghcr.io"); ok {
		t.Error("ghcr.io present after delete")
	}
	if ok, _ := s.Delete("ghcr.io"); ok {
		t.Error("second delete reported found")
	}
}

func TestStorePersistsAtMode600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := Open(path).Upsert("quay.io", "robot", "sekret"); err != nil {
		t.Fatal(err)
	}
	// A fresh Store over the same file sees the persisted cred.
	if c, ok := Open(path).Lookup("quay.io"); !ok || c.Secret != "sekret" {
		t.Fatalf("reopened lookup = %+v ok=%v", c, ok)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cred file mode = %o, want 600 (holds usable secrets)", fi.Mode().Perm())
	}
}

func TestUpsertRejectsEmptySecretAndDisabledStore(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "registry.json"))
	if err := s.Upsert("ghcr.io", "alice", ""); err == nil {
		t.Error("empty secret accepted")
	}
	if err := Open("").Upsert("ghcr.io", "alice", "tok"); err == nil {
		t.Error("upsert on a disabled (empty-path) store accepted")
	}
}

func TestKeychainResolvesStoredCredElseAnonymous(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "registry.json"))
	if err := s.Upsert("ghcr.io", "alice", "tok"); err != nil {
		t.Fatal(err)
	}
	kc := s.Keychain()

	// Known host → Basic auth with the stored cred.
	a, err := kc.Resolve(resource("ghcr.io"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "alice" || cfg.Password != "tok" {
		t.Errorf("resolved auth = %+v, want alice/tok", cfg)
	}

	// Unknown host → anonymous (public pulls unaffected).
	if a, _ := kc.Resolve(resource("example.com")); a != authn.Anonymous {
		t.Errorf("unknown host resolved to %v, want anonymous", a)
	}

	// A nil store yields an always-anonymous keychain.
	if a, _ := (*Store)(nil).Keychain().Resolve(resource("ghcr.io")); a != authn.Anonymous {
		t.Errorf("nil-store keychain resolved to %v, want anonymous", a)
	}
}

// resource is a minimal authn.Resource for a registry host.
type resource string

func (r resource) String() string      { return string(r) }
func (r resource) RegistryStr() string { return string(r) }
