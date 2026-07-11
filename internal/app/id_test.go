package app

import (
	"strings"
	"testing"
)

func TestNewIDShapeAndValidation(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "app_") {
		t.Errorf("id %q missing app_ prefix", id)
	}
	if !IsValidID(id) {
		t.Errorf("generated id %q fails IsValidID", id)
	}
	// Distinct across calls.
	id2, _ := NewID()
	if id == id2 {
		t.Error("two NewID calls returned the same id")
	}
}

func TestIsValidIDRejects(t *testing.T) {
	for _, bad := range []string{"", "app_", "sbx_abcd", "app_!!!", "nope"} {
		if IsValidID(bad) {
			t.Errorf("IsValidID(%q) = true, want false", bad)
		}
	}
}

func TestIsValidName(t *testing.T) {
	good := []string{"web", "api-server", "a", "my-app-2", strings.Repeat("a", 40)}
	for _, s := range good {
		if !IsValidName(s) {
			t.Errorf("IsValidName(%q) = false, want true", s)
		}
	}
	bad := []string{"", "-web", "web-", "Web", "web_1", "a.b", "café", strings.Repeat("a", 41)}
	for _, s := range bad {
		if IsValidName(s) {
			t.Errorf("IsValidName(%q) = true, want false", s)
		}
	}
}
