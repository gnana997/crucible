package network

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/network/dnsproxy"
)

// TestCheckNftSandboxIDRejectsInjection is the L6 guard: the sandbox id is
// concatenated into per-sandbox nft object names and the script fed to
// `nft -f -`, so anything outside [a-zA-Z0-9-] must be refused locally rather
// than trusting the distant sandbox-ID validator.
func TestCheckNftSandboxIDRejectsInjection(t *testing.T) {
	good := []string{"sbx-abc123", "a", strings.Repeat("a", 64)}
	for _, id := range good {
		if err := checkNftSandboxID(id); err != nil {
			t.Errorf("checkNftSandboxID(%q) = %v, want nil", id, err)
		}
	}
	bad := []string{
		"",                      // empty
		"sbx_abc",               // unsanitized underscore
		"a b",                   // space
		"a;drop",                // separator
		"a}\nadd element",       // nft-script injection attempt
		"a/b",                   // slash
		strings.Repeat("a", 65), // too long
	}
	for _, id := range bad {
		if err := checkNftSandboxID(id); err == nil {
			t.Errorf("checkNftSandboxID(%q) = nil, want error", id)
		}
	}
}

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

	// The input chain must accept established/related traffic before the
	// catch-all drop — that's the return path for the port-publish
	// forwarder's host-initiated connections into a guest. Without it,
	// the guest's SYN-ACK arrives on vh-* and is dropped. Anchor on the
	// input chain specifically (the forward chain has its own copy).
	inputIdx := strings.Index(got, "chain input {")
	inputCtIdx := strings.Index(got[inputIdx:], "ct state established,related accept")
	if inputIdx < 0 || inputCtIdx < 0 {
		t.Fatal("input chain missing established/related accept (port-publish return path)")
	}
	if inputIdx+inputCtIdx >= dropIdx {
		t.Errorf("input established-accept(%d) must precede vh-* drop(%d)", inputIdx+inputCtIdx, dropIdx)
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

// TestAllowIPsRejectsNonPublicAddresses is the belt-and-suspenders
// check for R1: AllowIPs is the last gate before an address lands in
// the nftables allow-set, and must refuse anything IsPublicUnicast
// rejects even if a caller somehow skipped the proxy's filter. The
// rejection path returns before shelling out to nft, so this needs
// no root.
func TestAllowIPsRejectsNonPublicAddresses(t *testing.T) {
	bad := []string{
		"169.254.169.254", // link-local cloud metadata
		"100.100.100.200", // Alibaba metadata (CGNAT)
		"100.64.0.1",      // CGNAT
		"10.20.0.2",       // sandbox pool / RFC1918
		"192.168.1.1",     // RFC1918
		"198.18.0.1",      // benchmarking
		"192.0.2.1",       // TEST-NET-1
		"240.0.0.1",       // reserved / future use
		"127.0.0.1",       // loopback
	}
	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			err := AllowIPs(context.Background(), "sbx-test", []dnsproxy.AllowedIP{
				{Addr: netip.MustParseAddr(s), TTL: time.Minute},
			})
			if err == nil {
				t.Errorf("AllowIPs accepted non-public address %s", s)
			}
		})
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
