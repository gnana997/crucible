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
	guest[3] += 2
	return Lease{
		Prefix:  p,
		Gateway: netip.AddrFrom4(gw),
		GuestIP: netip.AddrFrom4(guest),
	}
}

func TestVethSpecInterfaceNames(t *testing.T) {
	s := VethSpec{SandboxID: "sbx-abc"}
	// Names = prefix + ifaceSuffix(SandboxID). Compute the
	// expected hash at runtime so the assertion stays stable if
	// someone changes ifaceHashLen later.
	hash := ifaceSuffix("sbx-abc")
	if got, want := s.HostVeth(), "vh-"+hash; got != want {
		t.Errorf("HostVeth = %q, want %q", got, want)
	}
	if got, want := s.GuestVeth(), "vg-"+hash; got != want {
		t.Errorf("GuestVeth = %q, want %q", got, want)
	}
	if got, want := s.BridgeName(), "br-"+hash; got != want {
		t.Errorf("BridgeName = %q, want %q", got, want)
	}
	if got := s.Tap(); got != TapName {
		t.Errorf("Tap = %q, want fixed %q", got, TapName)
	}
	// All names must fit IFNAMSIZ.
	for _, n := range []string{s.HostVeth(), s.GuestVeth(), s.BridgeName(), s.Tap()} {
		if len(n) > ifnameMaxLen {
			t.Errorf("name %q exceeds IFNAMSIZ (%d > %d)", n, len(n), ifnameMaxLen)
		}
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
	// Regression guard: derived interface names must fit IFNAMSIZ
	// regardless of the sandbox ID's length. The hash-based
	// suffix makes this deterministic — any ID hashes to the same
	// number of hex chars — but the assertion catches us if
	// someone ever reverts to embedding the raw ID.
	veryLong := strings.Repeat("a", 128)
	s := VethSpec{SandboxID: veryLong}
	for _, name := range []string{s.HostVeth(), s.GuestVeth(), s.BridgeName(), s.Tap()} {
		if len(name) > ifnameMaxLen {
			t.Errorf("interface name %q exceeds IFNAMSIZ (%d > %d)",
				name, len(name), ifnameMaxLen)
		}
	}
}

func TestIfaceSuffixDeterministic(t *testing.T) {
	// Setup derives names from SandboxID; Teardown derives again
	// from the same SandboxID. They must match byte-for-byte or
	// we leak interfaces.
	for _, id := range []string{"sbx-abc", "sbx-r1qovql7buvo2", "x", strings.Repeat("z", 60)} {
		a := ifaceSuffix(id)
		b := ifaceSuffix(id)
		if a != b {
			t.Errorf("ifaceSuffix(%q) not deterministic: %q vs %q", id, a, b)
		}
		if len(a) != ifaceHashLen {
			t.Errorf("ifaceSuffix(%q) length = %d, want %d", id, len(a), ifaceHashLen)
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
