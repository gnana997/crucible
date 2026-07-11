package network

import (
	"net/netip"
	"testing"

	"github.com/gnana997/crucible/internal/network/dnsproxy"
)

func anyBlocked(a netip.Addr) bool {
	for _, p := range BlockedEgressPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// The nft-layer drop list must agree with the DNS-layer SSRF guard: everything
// we drop is non-public, and everything non-public that matters (metadata,
// RFC1918, CGNAT, reserved) is dropped — while real public hosts are not.
func TestBlockedEgressPrefixesAgreeWithPublicUnicast(t *testing.T) {
	// Every address inside a blocked prefix must be non-public.
	for _, p := range BlockedEgressPrefixes {
		for _, a := range []netip.Addr{p.Addr(), p.Masked().Addr().Next()} {
			if dnsproxy.IsPublicUnicast(a) {
				t.Errorf("%v (in blocked %v) is IsPublicUnicast — the guard disagrees", a, p)
			}
		}
	}

	// Known dangerous / private addresses must be both blocked AND non-public.
	for _, s := range []string{
		"169.254.169.254",                       // AWS/GCP/Azure metadata
		"100.100.100.200",                       // Alibaba metadata (CGNAT)
		"10.0.0.1", "172.16.5.5", "192.168.1.1", // RFC1918
		"127.0.0.1",       // loopback
		"0.0.0.0",         // this-network
		"255.255.255.255", // broadcast (240/4)
		"198.18.0.1",      // benchmarking
	} {
		a := netip.MustParseAddr(s)
		if !anyBlocked(a) {
			t.Errorf("%s is not in BlockedEgressPrefixes", s)
		}
		if dnsproxy.IsPublicUnicast(a) {
			t.Errorf("%s is unexpectedly IsPublicUnicast", s)
		}
	}

	// Real public hosts must be neither blocked nor rejected by the guard.
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34"} {
		a := netip.MustParseAddr(s)
		if anyBlocked(a) {
			t.Errorf("%s is wrongly in BlockedEgressPrefixes", s)
		}
		if !dnsproxy.IsPublicUnicast(a) {
			t.Errorf("%s should be IsPublicUnicast", s)
		}
	}
}
