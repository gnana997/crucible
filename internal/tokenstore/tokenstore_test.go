package tokenstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/policy"
)

func TestAddListVerifyRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")

	raw, e, err := Add(path, AddOptions{Name: "laptop"})
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
	if _, _, err := Add(path, AddOptions{Name: "x"}); err != nil {
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
	raw, _, err := Add(path, AddOptions{Name: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Verify(raw) {
		t.Error("store did not pick up the newly added key (reload failed)")
	}
}

func TestAddScopedTokenRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	pol := &policy.Policy{
		Operations:   []policy.Operation{policy.OpRead, policy.OpExec},
		MaxSandboxes: 4,
	}
	raw, e, err := Add(path, AddOptions{Name: "agent", Policy: pol})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Scoped() {
		t.Error("entry with a policy should be Scoped()")
	}

	// A fresh store reads the policy back from disk.
	idn, ok := Open(path).Identify(raw)
	if !ok {
		t.Fatal("Identify should accept the scoped key")
	}
	if idn.TokenID != e.ID {
		t.Errorf("Identify TokenID = %q, want %q", idn.TokenID, e.ID)
	}
	got := idn.Policy
	if got == nil {
		t.Fatal("expected a policy, got nil")
	}
	if got.MaxSandboxes != 4 || !got.Allows(policy.OpRead) || got.Allows(policy.OpCreate) {
		t.Errorf("policy round-trip wrong: %+v", got)
	}

	// The file actually persists the policy.
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"max_sandboxes": 4`) {
		t.Errorf("token file missing the policy: %s", b)
	}
}

func TestVerifyPolicyUnscoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	raw, _, err := Add(path, AddOptions{Name: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	idn, ok := Open(path).Identify(raw)
	if !ok {
		t.Fatal("unscoped key should verify")
	}
	if idn.Policy != nil {
		t.Errorf("unscoped key should carry a nil policy, got %+v", idn.Policy)
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	if !(Entry{ExpiresAt: &past}).Expired(now) {
		t.Error("a past expiry should be Expired")
	}
	if (Entry{ExpiresAt: &future}).Expired(now) {
		t.Error("a future expiry should not be Expired")
	}
	if (Entry{}).Expired(now) {
		t.Error("no expiry should never be Expired")
	}
}

func TestVerifyPolicyRejectsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	// Negative TTL mints an already-expired key (the CLI rejects negative TTLs;
	// this exercises the store's expiry check directly).
	rawExpired, _, err := Add(path, AddOptions{Name: "short", TTL: -time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Open(path).Identify(rawExpired); ok {
		t.Error("expired key must not verify")
	}
	if Open(path).Verify(rawExpired) {
		t.Error("expired key must not pass Verify either")
	}

	rawLive, _, err := Add(path, AddOptions{Name: "live", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Open(path).Identify(rawLive); !ok {
		t.Error("unexpired key should verify")
	}
}
