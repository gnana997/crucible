package secretstore

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func encodeKeyForTest(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func openTmp(t *testing.T, key []byte) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "secrets.db"), key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestBundleRoundTrip(t *testing.T) {
	s := openTmp(t, testKey(t))
	want := map[string]string{"DATABASE_URL": "postgres://x", "REDIS_URL": "redis://y"}
	if err := s.Set("web-env", want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, found, err := s.Get("web-env")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if len(got) != 2 || got["DATABASE_URL"] != "postgres://x" || got["REDIS_URL"] != "redis://y" {
		t.Fatalf("round-trip = %v, want %v", got, want)
	}
	if _, found, _ := s.Get("nope"); found {
		t.Error("absent bundle reported found")
	}
}

func TestSetKeysAndDeleteKey(t *testing.T) {
	s := openTmp(t, testKey(t))
	if err := s.Set("b", map[string]string{"A": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeys("b", map[string]string{"B": "2", "A": "11"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("b")
	if got["A"] != "11" || got["B"] != "2" {
		t.Fatalf("after SetKeys = %v, want A=11 B=2", got)
	}
	if err := s.DeleteKey("b", "A"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get("b")
	if _, ok := got["A"]; ok || got["B"] != "2" {
		t.Fatalf("after DeleteKey = %v, want only B=2", got)
	}
	if err := s.DeleteKey("missing", "A"); err != ErrNotFound {
		t.Errorf("DeleteKey on absent bundle = %v, want ErrNotFound", err)
	}
}

func TestListAndKeysNeverLeakValues(t *testing.T) {
	s := openTmp(t, testKey(t))
	_ = s.Set("b-two", map[string]string{"Y": "sekret", "X": "sekret"})
	_ = s.Set("a-one", map[string]string{"Z": "sekret"})

	names, err := s.List()
	if err != nil || len(names) != 2 || names[0] != "a-one" || names[1] != "b-two" {
		t.Fatalf("List = %v (err %v), want sorted [a-one b-two]", names, err)
	}
	keys, found, err := s.Keys("b-two")
	if err != nil || !found {
		t.Fatalf("Keys: found=%v err=%v", found, err)
	}
	if len(keys) != 2 || keys[0] != "X" || keys[1] != "Y" {
		t.Fatalf("Keys = %v, want sorted [X Y]", keys)
	}
	// The values must appear nowhere in the names/keys surfaces.
	for _, s := range append(names, keys...) {
		if s == "sekret" {
			t.Fatal("a value leaked into List/Keys output")
		}
	}
}

// A wrong master key can't open bundles a different key sealed.
func TestWrongKeyFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.db")
	s1, err := Open(path, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Set("b", map[string]string{"A": "1"})
	_ = s1.Close()

	s2, err := Open(path, testKey(t)) // different key, same file
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if _, _, err := s2.Get("b"); err == nil {
		t.Fatal("Get with the wrong key must fail (AEAD auth)")
	}
}

// GCM detects tampering, and the bundle name (AAD) binds a ciphertext to its
// name — a ciphertext moved to a different name won't open.
func TestTamperAndNameSwapFail(t *testing.T) {
	s := openTmp(t, testKey(t))
	rec, err := s.seal("a", map[string]string{"K": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.unseal("a", rec); err != nil {
		t.Fatalf("clean unseal failed: %v", err)
	}
	// Flip a ciphertext byte → auth fails.
	tampered := sealed{Nonce: rec.Nonce, Ciphertext: bytes.Clone(rec.Ciphertext)}
	tampered.Ciphertext[0] ^= 0xff
	if _, err := s.unseal("a", tampered); err == nil {
		t.Error("tampered ciphertext must fail to open")
	}
	// Same ciphertext, different name (AAD) → fails.
	if _, err := s.unseal("b", rec); err == nil {
		t.Error("a bundle sealed under \"a\" must not open under \"b\" (AAD binds the name)")
	}
}

func TestOpenRejectsShortKey(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "x.db"), []byte("short")); err == nil {
		t.Fatal("Open must reject a non-32-byte key")
	}
}

func TestInvalidBundleName(t *testing.T) {
	s := openTmp(t, testKey(t))
	for _, bad := range []string{"", "UPPER", "has space", "under_score", "-lead"} {
		if err := s.Set(bad, map[string]string{"A": "1"}); err == nil {
			t.Errorf("Set(%q) should be rejected", bad)
		}
	}
	if err := s.Set("ok-name9", map[string]string{"A": "1"}); err != nil {
		t.Errorf("Set(valid) rejected: %v", err)
	}
}

func TestLoadMasterKey(t *testing.T) {
	// Neither configured → disabled.
	t.Setenv(EnvKeyVar, "")
	if k, gen, err := LoadMasterKey(""); k != nil || gen || err != nil {
		t.Fatalf("no config = (%v,%v,%v), want (nil,false,nil) disabled", k, gen, err)
	}
	// File absent → generate + persist 0600.
	kf := filepath.Join(t.TempDir(), "key")
	k1, gen, err := LoadMasterKey(kf)
	if err != nil || !gen || len(k1) != KeySize {
		t.Fatalf("first load = gen %v err %v len %d; want generated 32-byte key", gen, err, len(k1))
	}
	if fi, _ := os.Stat(kf); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file perms = %v, want 0600", fi.Mode().Perm())
	}
	// Second load reads the same key back (not generated).
	k2, gen, err := LoadMasterKey(kf)
	if err != nil || gen || !bytes.Equal(k1, k2) {
		t.Fatalf("reload = gen %v err %v equal %v; want same key, not generated", gen, err, bytes.Equal(k1, k2))
	}
	// Env wins over the file.
	other := testKey(t)
	t.Setenv(EnvKeyVar, encodeKeyForTest(other))
	k3, _, err := LoadMasterKey(kf)
	if err != nil || !bytes.Equal(k3, other) {
		t.Fatalf("env key not used: err %v equal %v", err, bytes.Equal(k3, other))
	}
	// Bad env key errors.
	t.Setenv(EnvKeyVar, "not-base64!!")
	if _, _, err := LoadMasterKey(kf); err == nil {
		t.Error("invalid env key must error")
	}
}
