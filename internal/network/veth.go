package network

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
)

// Interface name constraints:
//
// Linux caps interface names at IFNAMSIZ (15 chars + NUL = 16).
// Sandbox IDs are 17 chars (prefix `sbx-` + 13 base32 random),
// which doesn't fit inside IFNAMSIZ even with a single-char
// prefix. Rather than forcing sandbox IDs to be shorter (they
// have other consumers — logs, URLs, netns names — that benefit
// from their current length), we hash the sandbox ID down to 8
// hex chars (32 bits, ~4B space) for the interface-name layer
// only. The readable sandbox ID lives alongside in the netns
// name, nft chain/set names, and every log line.
const (
	vethHostPrefix  = "vh-" // "veth host"
	vethGuestPrefix = "vg-"
	bridgePrefix    = "br-"

	// TapName is the single tap device name every sandbox netns
	// uses. Fixed (not per-sandbox) because Firecracker records
	// the host_dev_name in snapshot state; a fork restoring from
	// a snapshot would fail if the tap lived under a different
	// name in the fork's netns. Each sandbox has its own netns,
	// so "tap0" doesn't collide.
	TapName = "tap0"

	ifnameMaxLen = 15

	// ifaceHashLen is the number of hex chars we take from the
	// sandbox-ID hash. 8 hex = 32 bits of entropy, comfortable
	// for any realistic single-host sandbox concurrency.
	ifaceHashLen = 8
)

// ifaceSuffix derives a short, collision-resistant identifier
// embeddable in interface names within IFNAMSIZ. Deterministic so
// Teardown computes the same name as Setup without threading
// state.
func ifaceSuffix(sandboxID string) string {
	sum := sha1.Sum([]byte(sandboxID))
	return hex.EncodeToString(sum[:])[:ifaceHashLen]
}

// validIfaceSuffix restricts the characters that can follow our
// prefix. Matches jailer's sanitized ID alphabet.
var validIfaceSuffix = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

// VethSpec captures everything needed to stand up per-sandbox
// networking. The caller (sandbox.Manager in a later commit)
// computes these fields from the sandbox ID and the Lease.
type VethSpec struct {
	// SandboxID is the suffix used in interface names. Must already
	// be sanitized for use in iproute2 identifiers (jailer's
	// sanitizeJailerID produces compatible shapes). Length +
	// longest prefix must fit within Linux's 15-char IFNAMSIZ.
	SandboxID string

	// Netns is the network namespace name (without /var/run/netns
	// prefix) where the guest-side veth, bridge, and tap live.
	Netns string

	// Lease gives us the three addresses:
	//   Gateway: assigned to the host-side veth in root netns
	//   middle (/30 addr .2): assigned to the bridge inside netns
	//   GuestIP: the address DHCP will hand to the guest; lives
	//            on the guest's eth0, not on any host iface
	Lease Lease

	// DNSAnycast is the reserved host-side DNS address that every
	// sandbox reaches over its veth. We install a /32 route inside
	// the sandbox netns pointing to Gateway so the guest's DNS
	// traffic flows through the veth.
	DNSAnycast netip.Addr
}

// Validate checks invariants before any shell-out runs.
func (s VethSpec) Validate() error {
	if s.SandboxID == "" {
		return fmt.Errorf("network: VethSpec.SandboxID required")
	}
	if !validIfaceSuffix.MatchString(s.SandboxID) {
		return fmt.Errorf("network: VethSpec.SandboxID %q contains invalid characters", s.SandboxID)
	}
	if s.Netns == "" {
		return fmt.Errorf("network: VethSpec.Netns required")
	}
	if !s.Lease.Prefix.IsValid() {
		return fmt.Errorf("network: VethSpec.Lease has no prefix")
	}
	if !s.DNSAnycast.IsValid() {
		return fmt.Errorf("network: VethSpec.DNSAnycast required")
	}
	return nil
}

// Interface names derived from the sandbox ID via ifaceSuffix.
// Deterministic so Teardown reconstructs the same names as Setup.
// Length = prefix (3) + hash (8) = 11 chars, well within
// IFNAMSIZ.
func (s VethSpec) HostVeth() string   { return vethHostPrefix + ifaceSuffix(s.SandboxID) }
func (s VethSpec) GuestVeth() string  { return vethGuestPrefix + ifaceSuffix(s.SandboxID) }
func (s VethSpec) BridgeName() string { return bridgePrefix + ifaceSuffix(s.SandboxID) }

// Tap returns the fixed tap name every sandbox's netns uses. Same
// across sandboxes; netns isolation makes that safe and lets
// snapshot/restore work without host_dev_name rewriting.
func (s VethSpec) Tap() string { return TapName }

// Setup creates every host-side object for a sandbox, in order:
//
//  1. veth pair (host end + guest end, both in root netns)
//  2. Move guest end into the sandbox netns
//  3. Assign host-side IP (gateway) and bring it up
//  4. Inside the netns: bring up lo; create L3-less bridge;
//     enslave veth-g and tap to bridge; bring all up
//
// The bridge is intentionally address-less — a /30 only has two
// usable host addresses (.1 = gateway on the host-side veth,
// .2 = guest via DHCP) so there's no slot left for the bridge,
// and bridges don't need an L3 address to forward L2 frames.
// The DHCP responder reaches the bridge via SO_BINDTODEVICE, not
// by IP, so address-less is fine.
//
// The sandbox netns has no IPv4 routing table entries — it's a
// pure L2 bridge. All L3 handling (DNS anycast dummy iface,
// masquerade, forward filter) lives in root netns.
//
// On any error, Setup rolls back the host-visible veth (deleting
// the host-side end auto-removes the guest-side end too, and the
// caller is expected to delete the netns separately as part of
// full teardown).
//
// Setup assumes the netns already exists (created by CreateNetns).
func Setup(ctx context.Context, spec VethSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	var success bool
	defer func() {
		if !success {
			// Best-effort rollback of the veth pair. Teardown of
			// the netns and anything inside it is the caller's
			// job — we can't blow away a user-provided netns on
			// our error path.
			_ = runCmd(context.Background(), "ip", "link", "delete", spec.HostVeth())
		}
	}()

	// 1. veth pair in root netns.
	if err := runCmd(ctx, "ip", "link", "add", spec.HostVeth(),
		"type", "veth", "peer", "name", spec.GuestVeth()); err != nil {
		return fmt.Errorf("create veth pair: %w", err)
	}

	// 2. Move guest end into the sandbox netns.
	if err := runCmd(ctx, "ip", "link", "set", spec.GuestVeth(),
		"netns", spec.Netns); err != nil {
		return fmt.Errorf("move veth-g to netns: %w", err)
	}

	// 3. Host-side IP + up.
	gwWithPrefix := fmt.Sprintf("%s/%d", spec.Lease.Gateway, spec.Lease.Prefix.Bits())
	if err := runCmd(ctx, "ip", "addr", "add", gwWithPrefix, "dev", spec.HostVeth()); err != nil {
		return fmt.Errorf("assign host-side IP: %w", err)
	}
	if err := runCmd(ctx, "ip", "link", "set", spec.HostVeth(), "up"); err != nil {
		return fmt.Errorf("bring up host-side veth: %w", err)
	}

	// 4. Inside-netns setup. Use `ip netns exec` for each step;
	// subprocess overhead is cheap at per-sandbox-create frequency.
	nsExec := func(args ...string) []string {
		return append([]string{"ip", "netns", "exec", spec.Netns}, args...)
	}

	// loopback up (dhclient needs it for lease-file bookkeeping).
	if err := runCmd(ctx, nsExec("ip", "link", "set", "lo", "up")...); err != nil {
		return fmt.Errorf("bring up lo in netns: %w", err)
	}

	// Create the tap device.
	if err := runCmd(ctx, nsExec("ip", "tuntap", "add", spec.Tap(), "mode", "tap")...); err != nil {
		return fmt.Errorf("create tap: %w", err)
	}

	// Create the bridge L3-less. A /30 only has two usable host
	// addresses; .1 is the gateway on the host-side veth and .2
	// is the guest via DHCP — no slot left for the bridge, and
	// bridges don't need an L3 address to forward L2 frames.
	if err := runCmd(ctx, nsExec("ip", "link", "add", spec.BridgeName(), "type", "bridge")...); err != nil {
		return fmt.Errorf("create bridge: %w", err)
	}

	// Enslave veth-g and tap to the bridge; bring everything up.
	for _, iface := range []string{spec.GuestVeth(), spec.Tap()} {
		if err := runCmd(ctx, nsExec("ip", "link", "set", iface, "master", spec.BridgeName())...); err != nil {
			return fmt.Errorf("attach %s to bridge: %w", iface, err)
		}
	}
	for _, iface := range []string{spec.GuestVeth(), spec.Tap(), spec.BridgeName()} {
		if err := runCmd(ctx, nsExec("ip", "link", "set", iface, "up")...); err != nil {
			return fmt.Errorf("bring up %s: %w", iface, err)
		}
	}

	// Sandbox netns has no routing table entries of its own — it's
	// a pure L2 bridge. The guest receives its default gateway and
	// DNS server from DHCP and reaches 10.20.255.254 via its own
	// eth0 → tap0 → bridge → veth-g → vh-XXX path. Packets only
	// hit L3 in root netns, where the anycast dummy iface + nft
	// rules live.

	success = true
	return nil
}

// Teardown removes the host-side veth. The guest-side veth, bridge,
// and tap live inside the netns and are destroyed by the kernel
// when the caller deletes the netns (via DeleteNetns) — so we
// don't attempt to remove them individually.
//
// Missing host-side veth is treated as success (idempotent).
func Teardown(ctx context.Context, spec VethSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	err := runCmd(ctx, "ip", "link", "delete", spec.HostVeth())
	if err == nil {
		return nil
	}
	// `ip link delete` on a missing link exits with "Cannot find
	// device". We swallow that to stay idempotent.
	if isCannotFindDevice(err) {
		return nil
	}
	return err
}

// isCannotFindDevice inspects an error returned from runCmd to
// decide whether it represents "already gone" — the only failure
// mode Teardown wants to treat as success.
func isCannotFindDevice(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsAny(msg,
		"Cannot find device",
		"does not exist",
		"No such device",
	)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

// indexOf is a tiny substring search — avoids pulling strings.Contains
// on the hot Teardown path and keeps this file's imports minimal.
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
