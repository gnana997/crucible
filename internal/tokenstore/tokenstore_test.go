package tokenstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddListVerifyRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")

	raw, e, err := Add(path, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "crucible_") {
		t.Errorf("raw key = %q, want a crucible_ prefix", raw)
	}
	if e.ID == "" || e.Name != "laptop" {
		t.Errorf("entry = %+v", e)
	}

	entries, err := List(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != e.ID {
		t.Fatalf("list = %+v", entries)
	}

	// The file stores the hash, never the raw key.
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), raw) {
		t.Error("token file contains the raw key")
	}
	if !strings.Contains(string(b), e.Hash) {
		t.Error("token file is missing the hash")
	}

	st := Open(path)
	if !st.Enabled() {
		t.Error("Enabled() = false, want true")
	}
	if !st.Verify(raw) {
		t.Error("Verify(raw) = false, want true")
	}
	if st.Verify("crucible_wrong") {
		t.Error("Verify(wrong) = true, want false")
	}
	if st.Verify("") {
		t.Error(`Verify("") = true, want false`)
	}

	ok, err := Revoke(path, e.ID)
	if err != nil || !ok {
		t.Fatalf("Revoke: ok=%v err=%v", ok, err)
	}
	// The same Store reloads (mtime+size change) and now rejects the key.
	if st.Verify(raw) {
		t.Error("Verify(raw) after revoke = true, want false")
	}
	if st.Enabled() {
		t.Error("Enabled() after revoke = true, want false")
	}
}

func TestRevokeUnknownID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if _, _, err := Add(path, "x"); err != nil {
		t.Fatal(err)
	}
	ok, err := Revoke(path, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Revoke(unknown) = true, want false")
	}
}

func TestStoreMissingFileIsEmpty(t *testing.T) {
	st := Open(filepath.Join(t.TempDir(), "nope.json"))
	if st.Enabled() {
		t.Error("Enabled() on a missing file = true, want false")
	}
	if st.Verify("anything") {
		t.Error("Verify on a missing file = true")
	}
}

func TestStoreReloadsOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	st := Open(path)
	if st.Enabled() {
		t.Fatal("Enabled() before any key = true")
	}
	raw, _, err := Add(path, "k")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Verify(raw) {
		t.Error("store did not pick up the newly added key (reload failed)")
	}
}
