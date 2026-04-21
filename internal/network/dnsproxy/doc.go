// Package dnsproxy is the DNS enforcement layer for crucible's
// default-deny network feature.
//
// One shared instance runs in the host root netns, bound to a
// reserved anycast IP (typically 10.20.255.254:53). Every sandbox
// routes DNS to that address via a /32 route installed in its own
// netns, so all sandboxes share the same listener. The proxy uses
// the source IP of each incoming packet — kernel-attested because
// nft ingress rules drop spoofed sources — to identify which
// sandbox sent the query and which per-sandbox allowlist applies.
//
// Request handling:
//
//  1. Look up the policy by source IP in a sync.Map. Unknown
//     sources (no sandbox registered for that IP) are dropped
//     silently — no reply, no log-spam.
//  2. For each question, consult the policy's Allowlist. If any
//     question is denied, return NXDOMAIN for the whole query.
//  3. Forward the allowed query to the configured upstream
//     resolver using miekg/dns's Client (which handles EDNS0 +
//     TCP fallback on truncation for free).
//  4. Walk the answer section. For every A record, call the
//     caller-provided AllowIP hook so the sandbox's nftables set
//     gets populated with the resolved address + its TTL.
//  5. Return the upstream's reply to the guest.
//
// Package name is "dnsproxy" rather than "dns" to avoid colliding
// with miekg/dns at import sites.
package dnsproxy
