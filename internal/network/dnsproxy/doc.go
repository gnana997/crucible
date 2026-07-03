// Package dnsproxy is the DNS enforcement layer for crucible's
// default-deny network feature.
//
// One shared instance runs in the host root netns, bound to a
// reserved anycast IP (typically 10.20.255.254:53). Every sandbox
// routes DNS to that address via a /32 route installed in its own
// netns, so all sandboxes share the same listener. The proxy uses
// the source IP of each incoming packet to identify which sandbox
// sent the query and which per-sandbox allowlist applies. That
// source is kernel-attested: the nft input chain only accepts DNS
// packets whose (ingress veth, source IP) pair is registered in
// the guest_sources set (see network.BuildBaseScript), so a guest
// spoofing another sandbox's address is dropped before the proxy
// ever sees the packet.
//
// Request handling:
//
//  1. Shed load: a global inflight cap bounds total handler
//     concurrency, a per-source in-flight cap bounds any one
//     sandbox's share of it (so a slow/stalling upstream can't let
//     one guest starve the rest), and a per-source token bucket
//     bounds each sandbox's query rate. Packets beyond any limit are
//     dropped silently.
//  2. Look up the policy by source IP in a sync.Map. Unknown
//     sources (no sandbox registered for that IP) are dropped
//     silently — no reply, no log-spam.
//  3. For each question, consult the policy's Allowlist. If any
//     question is denied, return NXDOMAIN for the whole query.
//  4. Forward the allowed query to the configured upstream
//     resolver using miekg/dns's Client (which handles EDNS0 +
//     TCP fallback on truncation for free).
//  5. Vet the answer section. AAAA records are stripped (the
//     sandbox network is IPv4-only), as is any A record whose
//     address is not public global-unicast (see IsPublicUnicast) —
//     link-local (cloud metadata 169.254.169.254), loopback,
//     RFC1918/sandbox-pool, CGNAT (Alibaba metadata 100.100.100.200),
//     multicast, broadcast, and the other IANA special-purpose
//     ranges — or inside a configured blocked prefix. An
//     attacker-controlled upstream must not be able to open
//     egress to the host, another tenant, or the LAN. At most
//     maxAnswerIPs A records survive per reply.
//  6. Call the caller-provided AllowIP hook once with every
//     surviving A record + TTL so the sandbox's nftables set is
//     updated in a single batched nft invocation.
//  7. Return the vetted reply to the guest.
//
// Package name is "dnsproxy" rather than "dns" to avoid colliding
// with miekg/dns at import sites.
package dnsproxy
