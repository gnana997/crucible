package tokenstore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/policy"
)

func TestBoltStoreCreateIdentifyRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.db")
	s, err := OpenBolt(path)
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}

	if s.Enabled() {
		t.Fatal("empty store should not be Enabled")
	}

	pol := &policy.Policy{Operations: []policy.Operation{policy.OpRead}}
	raw, e, err := s.Create(AddOptions{Name: "user-42", Policy: pol})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if raw == "" || e.ID == "" {
		t.Fatal("Create returned empty key/id")
	}
	if !s.Enabled() {
		t.Fatal("store should be Enabled after Create")
	}

	// the raw key identifies to the scoped policy; a bogus key does not.
	id, ok := s.Identify(raw)
	if !ok || id.TokenID != e.ID || id.Policy == nil {
		t.Fatalf("Identify(raw) = %+v, %v", id, ok)
	}
	if _, ok := s.Identify("crucible_bogus"); ok {
		t.Fatal("bogus key should not Identify")
	}

	// list shows the entry (no secret leaked: Hash != raw).
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Hash == raw {
		t.Fatalf("List = %v, %v", list, err)
	}

	// persists across reopen.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := OpenBolt(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if _, ok := s2.Identify(raw); !ok {
		t.Fatal("key should survive reopen")
	}

	// revoke removes it.
	if ok, err := s2.Revoke(e.ID); err != nil || !ok {
		t.Fatalf("Revoke = %v, %v", ok, err)
	}
	if _, ok := s2.Identify(raw); ok {
		t.Fatal("revoked key should not Identify")
	}
	if ok, _ := s2.Revoke("nope"); ok {
		t.Fatal("revoking unknown id should return false")
	}
}

func TestBoltStoreExpiry(t *testing.T) {
	s, err := OpenBolt(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	defer func() { _ = s.Close() }()
	raw, _, err := s.Create(AddOptions{Name: "short", TTL: -time.Second}) // already expired
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := s.Identify(raw); ok {
		t.Fatal("expired key should not Identify")
	}
}
