package network

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
)

// Interface name constraints:
//   - Linux caps interface names at 15 chars (IFNAMSIZ = 16 with
//     the terminator). We keep our prefixes short to leave room
//     for the sandbox ID suffix.
//   - The sandbox ID is sanitized (underscores → hyphens, max 64
//     chars); we truncate here if needed and ensure uniqueness
//     by construction since IDs are random base32.
const (
	vethHostPrefix   = "vh-" // "veth host"; 3 chars + id → 12 chars budget for id
	vethGuestPrefix  = "vg-"
	bridgePrefix     = "br-"
	tapPrefix        = "tap-"
	ifnameMaxLen     = 15
	idMaxLenForIface = ifnameMaxLen - 4 // "vh-" is 3, "tap-" is 4 — use the larger
)

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
	if len(s.SandboxID) > idMaxLenForIface {
		return fmt.Errorf("network: VethSpec.SandboxID %q too long for interface names (max %d chars)",
			s.SandboxID, idMaxLenForIface)
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

// Interface names derived from the sandbox ID. Deterministic so
// teardown can compute them from the same spec.
func (s VethSpec) HostVeth() string   { return vethHostPrefix + s.SandboxID }
func (s VethSpec) GuestVeth() string  { return vethGuestPrefix + s.SandboxID }
func (s VethSpec) BridgeName() string { return bridgePrefix + s.SandboxID }
func (s VethSpec) TapName() string    { return tapPrefix + s.SandboxID }

// BridgeIP is the /30 address .2 (neither gateway nor guest). We
// assign it to the bridge inside the netns so the bridge is
// L3-addressable, which keeps the kernel happy when forwarding
// between veth-g and tap.
func (s VethSpec) BridgeIP() netip.Addr {
	gw := s.Lease.Gateway
	b := gw.As4()
	b[3]++ // .1 → .2
	return netip.AddrFrom4(b)
}

// Setup creates every host-side object for a sandbox, in order:
//
//  1. veth pair (host end + guest end, both in root netns)
//  2. Move guest end into the sandbox netns
//  3. Assign host-side IP (gateway) and bring it up
//  4. Inside the netns: bring up lo; assign bridge IP; create
//     bridge; enslave veth-g and tap to bridge; bring all up
//  5. Inside the netns: route the DNS anycast IP via the gateway
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
	if err := runCmd(ctx, nsExec("ip", "tuntap", "add", spec.TapName(), "mode", "tap")...); err != nil {
		return fmt.Errorf("create tap: %w", err)
	}

	// Create the bridge and assign it the .2 address. L3 addr on
	// the bridge lets the kernel route between veth-g and tap even
	// though we only want L2 forwarding; leaving the bridge
	// address-less works on most kernels but is fragile.
	if err := runCmd(ctx, nsExec("ip", "link", "add", spec.BridgeName(), "type", "bridge")...); err != nil {
		return fmt.Errorf("create bridge: %w", err)
	}
	brAddr := fmt.Sprintf("%s/%d", spec.BridgeIP(), spec.Lease.Prefix.Bits())
	if err := runCmd(ctx, nsExec("ip", "addr", "add", brAddr, "dev", spec.BridgeName())...); err != nil {
		return fmt.Errorf("assign bridge IP: %w", err)
	}

	// Enslave veth-g and tap to the bridge; bring everything up.
	for _, iface := range []string{spec.GuestVeth(), spec.TapName()} {
		if err := runCmd(ctx, nsExec("ip", "link", "set", iface, "master", spec.BridgeName())...); err != nil {
			return fmt.Errorf("attach %s to bridge: %w", iface, err)
		}
	}
	for _, iface := range []string{spec.GuestVeth(), spec.TapName(), spec.BridgeName()} {
		if err := runCmd(ctx, nsExec("ip", "link", "set", iface, "up")...); err != nil {
			return fmt.Errorf("bring up %s: %w", iface, err)
		}
	}

	// 5. Route the DNS anycast IP via the gateway. Without this
	// the guest's queries to the anycast would have no route.
	if err := runCmd(ctx, nsExec("ip", "route", "add",
		fmt.Sprintf("%s/32", spec.DNSAnycast), "via", spec.Lease.Gateway.String(),
	)...); err != nil {
		return fmt.Errorf("add anycast route: %w", err)
	}

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
