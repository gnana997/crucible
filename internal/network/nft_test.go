package network

import (
	"net/netip"
	"strings"
	"testing"
)

func TestBuildBaseScriptContainsExpectedChains(t *testing.T) {
	got := BuildBaseScript("eth0", netip.MustParseAddr("10.20.255.254"))
	mustContainAll(t, got, []string{
		"table inet crucible {",
		"map sandbox_chains {",
		"type ifname : verdict",
		"set guest_sources {",
		"type ifname . ipv4_addr",
		"chain forward {",
		"policy drop",
		"ct state established,related accept",
		"iifname vmap @sandbox_chains",
		"chain input {",
		"type filter hook input priority 0; policy accept;",
		"iifname . ip saddr @guest_sources ip daddr 10.20.255.254 udp dport 53 accept",
		"iifname \"vh-*\" drop",
		"chain postrouting {",
		"oifname \"eth0\" masquerade",
	})

	// The input chain's DNS accept must come before its catch-all
	// drop, or every guest query is dropped.
	acceptIdx := strings.Index(got, "iifname . ip saddr @guest_sources")
	dropIdx := strings.Index(got, "iifname \"vh-*\" drop")
	if acceptIdx < 0 || dropIdx < 0 || acceptIdx >= dropIdx {
		t.Errorf("input chain rule order wrong: accept(%d) drop(%d)", acceptIdx, dropIdx)
	}
}

func TestBuildSandboxScript(t *testing.T) {
	anycast := netip.MustParseAddr("10.20.255.254")
	guestIP := netip.MustParseAddr("10.20.0.2")
	got := BuildSandboxScript("sbx-abc", "vh-sbx-abc", guestIP, anycast)

	mustContainAll(t, got, []string{
		"add set inet crucible sandbox_sbx-abc_allowed",
		"type ipv4_addr",
		"flags timeout",
		"add chain inet crucible sandbox_sbx-abc",
		"add rule inet crucible sandbox_sbx-abc ip daddr 10.20.255.254 udp dport 53 accept",
		"add rule inet crucible sandbox_sbx-abc ip daddr @sandbox_sbx-abc_allowed accept",
		"add element inet crucible sandbox_chains { \"vh-sbx-abc\" : jump sandbox_sbx-abc }",
		"add element inet crucible guest_sources { \"vh-sbx-abc\" . 10.20.0.2 }",
	})
}

func TestBuildSandboxTeardownScript(t *testing.T) {
	got := BuildSandboxTeardownScript("sbx-abc", "vh-sbx-abc", netip.MustParseAddr("10.20.0.2"))
	// The guest_sources pair and map entry must be removed before
	// the chain — otherwise new packets are accepted/dispatched
	// into a soon-dead chain.
	srcIdx := strings.Index(got, "delete element inet crucible guest_sources { \"vh-sbx-abc\" . 10.20.0.2 }")
	mapIdx := strings.Index(got, "delete element inet crucible sandbox_chains")
	chainIdx := strings.Index(got, "delete chain inet crucible sandbox_sbx-abc")
	setIdx := strings.Index(got, "delete set inet crucible sandbox_sbx-abc_allowed")
	if srcIdx < 0 || mapIdx < 0 || chainIdx < 0 || setIdx < 0 {
		t.Fatalf("teardown script missing expected lines:\n%s", got)
	}
	if srcIdx >= chainIdx || mapIdx >= chainIdx || chainIdx >= setIdx {
		t.Errorf("teardown order wrong: sources(%d) map(%d) chain(%d) set(%d)", srcIdx, mapIdx, chainIdx, setIdx)
	}
}

func TestSandboxNamesUseSandboxID(t *testing.T) {
	// Cheap consistency check — the two derived names stay in
	// sync so orphan-reap by prefix covers both.
	if !strings.HasPrefix(sandboxChainName("x"), "sandbox_") {
		t.Error("chain name prefix changed")
	}
	if !strings.HasPrefix(sandboxAllowedSetName("x"), "sandbox_") {
		t.Error("set name prefix changed")
	}
	if !strings.HasSuffix(sandboxAllowedSetName("x"), "_allowed") {
		t.Error("set name suffix changed")
	}
}

func mustContainAll(t *testing.T, s string, subs []string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("expected substring %q not found in:\n%s", sub, s)
		}
	}
}
