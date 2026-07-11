package network

import (
	"net/netip"

	"github.com/gnana997/crucible/internal/network/dnsproxy"
)

// BlockedEgressPrefixes are the IPv4 ranges the range-based egress modes
// (full-egress and CIDR allowlists) must DROP before accepting anything. They
// are exactly the addresses dnsproxy.IsPublicUnicast rejects, expressed as CIDRs
// so nftables can enforce them on the forward path.
//
// This is the nft-layer half of the SSRF guard. The hostname-allowlist path
// never needs it (the DNS proxy only ever adds public-unicast IPs to a
// sandbox's allow-set), but the moment we accept egress by *range* — all public
// hosts, or an operator-supplied CIDR — the guest could otherwise reach RFC1918,
// link-local (cloud metadata 169.254.169.254), CGNAT (100.100.100.200), or a
// reserved block. Dropping these first closes that.
//
// blocked_prefixes_test.go asserts this list agrees with IsPublicUnicast, so the
// nft-layer guard can never drift from the DNS-layer one.
var BlockedEgressPrefixes = func() []netip.Prefix {
	// Caught by IsGlobalUnicast/IsPrivate at the per-IP level, listed here as
	// CIDRs for nft: RFC1918, loopback, link-local, and multicast.
	base := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918
		netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918
		netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
		netip.MustParsePrefix("127.0.0.0/8"),    // loopback
		netip.MustParsePrefix("169.254.0.0/16"), // link-local (incl. 169.254.169.254 metadata)
		netip.MustParsePrefix("224.0.0.0/4"),    // multicast
	}
	// Plus the "looks public but isn't" reserved blocks (0.0.0.0/8, CGNAT
	// 100.64/10, TEST-NETs, benchmarking, 240/4 incl. 255.255.255.255, …) —
	// the exact set IsPublicUnicast rejects beyond private/link-local.
	return append(base, dnsproxy.ReservedV4Prefixes()...)
}()
