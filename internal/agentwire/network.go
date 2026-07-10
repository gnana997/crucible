package agentwire

// NetworkConfigRequest is the JSON body of POST /network/configure: a
// complete static network configuration the host pushes to a guest that
// can't self-configure (OCI images have no DHCP client). The agent
// applies it with netlink — no `ip` binary needed — and writes
// resolv.conf/hosts/hostname. Applied at create and re-applied on fork
// (a fork's new /30 changes the address), so it is idempotent: the
// agent flushes eth0's existing IPv4 addresses and default route first.
type NetworkConfigRequest struct {
	// Interface is the guest NIC to configure. Empty means "eth0".
	Interface string `json:"interface,omitempty"`

	// Address is the guest's IPv4 address (no prefix), e.g.
	// "10.20.0.14". PrefixLen is its network prefix length (30 for a
	// /30).
	Address   string `json:"address"`
	PrefixLen int    `json:"prefix_len"`

	// Gateway is the default route's next hop (the host-side veth),
	// e.g. "10.20.0.13".
	Gateway string `json:"gateway"`

	// DNS is the resolver(s) written to /etc/resolv.conf — the shared
	// DNS-proxy anycast address for crucible.
	DNS []string `json:"dns,omitempty"`

	// Hostname is set on the kernel and written to /etc/hostname and
	// /etc/hosts.
	Hostname string `json:"hostname,omitempty"`
}
