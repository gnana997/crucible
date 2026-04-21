package network

import (
	"strings"
	"testing"
)

func TestNetnsPathMatchesIproute2Convention(t *testing.T) {
	if got, want := NetnsPath("crucible-x"), "/var/run/netns/crucible-x"; got != want {
		t.Errorf("NetnsPath = %q, want %q", got, want)
	}
}

func TestValidNetnsNameAcceptsExpected(t *testing.T) {
	ok := []string{
		"crucible-sbx-abc",
		"crucible-abc-12345",
		"crucible_x",
		"x",
		strings.Repeat("a", 64),
	}
	for _, n := range ok {
		if !validNetnsName.MatchString(n) {
			t.Errorf("%q should be valid", n)
		}
	}
}

func TestValidNetnsNameRejectsBad(t *testing.T) {
	bad := []string{
		"",
		"name with space",
		"name/with/slash",
		"name;with;semicolon",
		"name.with.dot",
		"too-" + strings.Repeat("a", 64), // 68 chars
	}
	for _, n := range bad {
		if validNetnsName.MatchString(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}
