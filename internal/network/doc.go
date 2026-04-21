// Package network owns the default-deny-with-per-sandbox-allowlist
// egress story for crucible sandboxes.
//
// The full architecture is documented in docs/network.md; this file
// is the orientation guide for readers jumping into the code.
//
// The package is organized into layers, each landed as its own
// commit so reviewers can follow the construction:
//
//  1. Pure data structures: subnet.Pool allocates
//     non-overlapping /30 blocks out of a configurable base CIDR;
//     Allowlist parses user-supplied hostname patterns and answers
//     Matches(name) queries in O(labels). Neither touches IO, so
//     both are safe to import from anywhere.
//
//  2. Host-side network plumbing: shell-out wrappers
//     for `ip netns`, `ip link`, `nft`, plus a hand-rolled
//     per-netns DHCP responder.
//
//  3. DNS proxy: single UDP listener bound to the reserved anycast
//     IP in host root netns; multiplexes by guest source IP via a
//     sync.Map to identify the originating sandbox in O(1).
//
//  4. Agent-side network refresh endpoint, plumbed through to
//     sandbox.Manager.Fork so forked VMs pick up their fork-private
//     IP immediately on resume rather than waiting for dhclient's
//     next renewal cycle.
//
//  5. sandbox.Manager integration and the createSandboxRequest
//     wire contract (`network: {enabled, allowlist}`).
//
//  6. Orphan reap on daemon startup + end-to-end smoke script.
package network
