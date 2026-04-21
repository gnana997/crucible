package network

import (
	"net/netip"
	"strings"
	"testing"
)

func makeLease(t *testing.T, prefix string) Lease {
	t.Helper()
	p, err := netip.ParsePrefix(prefix)
	if err != nil {
		t.Fatal(err)
	}
	gw := p.Addr().As4()
	gw[3]++
	guest := p.Addr().As4()
	guest[3] += 3
	return Lease{
		Prefix:  p,
		Gateway: netip.AddrFrom4(gw),
		GuestIP: netip.AddrFrom4(guest),
	}
}

func TestVethSpecInterfaceNames(t *testing.T) {
	s := VethSpec{SandboxID: "sbx-abc"}
	if got := s.HostVeth(); got != "vh-sbx-abc" {
		t.Errorf("HostVeth = %q", got)
	}
	if got := s.GuestVeth(); got != "vg-sbx-abc" {
		t.Errorf("GuestVeth = %q", got)
	}
	if got := s.BridgeName(); got != "br-sbx-abc" {
		t.Errorf("BridgeName = %q", got)
	}
	if got := s.TapName(); got != "tap-sbx-abc" {
		t.Errorf("TapName = %q", got)
	}
}

func TestVethSpecBridgeIP(t *testing.T) {
	s := VethSpec{
		Lease: makeLease(t, "10.20.7.0/30"),
	}
	if got, want := s.BridgeIP().String(), "10.20.7.2"; got != want {
		t.Errorf("BridgeIP = %s, want %s", got, want)
	}
}

func TestVethSpecValidate(t *testing.T) {
	base := VethSpec{
		SandboxID:  "sbx-abc",
		Netns:      "crucible-sbx-abc",
		Lease:      makeLease(t, "10.20.0.0/30"),
		DNSAnycast: netip.MustParseAddr("10.20.255.254"),
	}
	cases := []struct {
		name    string
		mutate  func(*VethSpec)
		wantErr string
	}{
		{name: "valid", mutate: func(*VethSpec) {}},
		{
			name:    "empty id",
			mutate:  func(s *VethSpec) { s.SandboxID = "" },
			wantErr: "SandboxID required",
		},
		{
			name:    "bad id chars",
			mutate:  func(s *VethSpec) { s.SandboxID = "bad id" },
			wantErr: "invalid characters",
		},
		{
			name:    "id too long",
			mutate:  func(s *VethSpec) { s.SandboxID = strings.Repeat("a", idMaxLenForIface+1) },
			wantErr: "too long",
		},
		{
			name:    "empty netns",
			mutate:  func(s *VethSpec) { s.Netns = "" },
			wantErr: "Netns required",
		},
		{
			name:    "invalid prefix",
			mutate:  func(s *VethSpec) { s.Lease = Lease{} },
			wantErr: "no prefix",
		},
		{
			name:    "invalid anycast",
			mutate:  func(s *VethSpec) { s.DNSAnycast = netip.Addr{} },
			wantErr: "DNSAnycast required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			err := s.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q missing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestIfnameBudget(t *testing.T) {
	// Sanity: our longest prefix + max ID suffix must still fit
	// in Linux's IFNAMSIZ. If this ever fails, shorten prefixes.
	maxID := strings.Repeat("a", idMaxLenForIface)
	s := VethSpec{SandboxID: maxID}
	for _, name := range []string{s.HostVeth(), s.GuestVeth(), s.BridgeName(), s.TapName()} {
		if len(name) > ifnameMaxLen {
			t.Errorf("interface name %q exceeds IFNAMSIZ (%d > %d)", name, len(name), ifnameMaxLen)
		}
	}
}

func TestIndexOfAndContainsAny(t *testing.T) {
	// Our tiny substring helper stays honest: empty pattern
	// always matches at 0; pattern longer than string can't
	// match; simple case works.
	if indexOf("hello", "") != 0 {
		t.Error("empty pattern should match at 0")
	}
	if indexOf("hi", "hello") != -1 {
		t.Error("pattern longer than string should not match")
	}
	if indexOf("Cannot find device", "find device") == -1 {
		t.Error("substring should be found")
	}
	if !containsAny("Cannot find device", "Does not exist", "find device") {
		t.Error("containsAny should return true for second match")
	}
	if containsAny("nothing matches", "x", "y", "z") {
		t.Error("containsAny should return false when no substring matches")
	}
}
