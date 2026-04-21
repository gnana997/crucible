// Package dhcp is a minimal DHCPv4 responder sized for crucible's
// narrow use case: one server per network namespace, one known
// client MAC, one pre-computed lease. It's deliberately hand-
// rolled rather than using an off-the-shelf library; the protocol
// is frozen (RFC 2131 from 1997) and small enough that owning the
// parsing code is clearer than pulling in a general-purpose DHCP
// implementation we'd use 5% of.
//
// The responder handles two message-type pairs:
//
//   DHCPDISCOVER → DHCPOFFER    fresh guest booting for the first time
//   DHCPREQUEST  → DHCPACK       guest confirming the offer
//
// On a REQUEST whose ciaddr or requested-IP option doesn't match
// the configured lease — typical for a forked VM whose kernel
// restored from a snapshot and thinks it still has the source's
// IP — we reply DHCPNAK, which forces dhclient to restart the
// discovery cycle and land on the fork's correct address.
//
// The responder enters its target netns via runtime.LockOSThread
// + unix.Setns(CLONE_NEWNET) before binding UDP/67, so the socket
// belongs to the netns for its lifetime regardless of where the
// Go runtime later schedules other goroutines.
//
// See docs/network.md for how this plugs into the broader network
// feature; see RFC 2131 for the protocol this code implements.
package dhcp
