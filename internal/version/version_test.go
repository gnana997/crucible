package version

import "testing"

func TestStringNonEmpty(t *testing.T) {
	if got := String(); got == "" {
		t.Fatalf("String() returned empty; want a non-empty version identifier")
	}
}
