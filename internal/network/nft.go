package network

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// NftTableName is the single inet-family nftables table every
// crucible rule lives inside. One table with a well-known name
// means: nuking it is atomic, listing our rules is `nft list
// table inet crucible`, and we can't collide with other tables
// operators might have.
const NftTableName = "crucible"

// nft chain and map names, derived from the table name.
const (
	nftForwardChain     = "forward"
	nftPostroutingChain = "postrouting"
	nftSandboxMap       = "sandbox_chains" // iifname verdict map
)

// sandboxChainName returns the per-sandbox chain name. All
// per-sandbox nft objects (chain, allowed-IPs set) are named by
// concatenation so we can list and clean them up by prefix.
func sandboxChainName(sandboxID string) string {
	return "sandbox_" + sandboxID
}

func sandboxAllowedSetName(sandboxID string) string {
	return "sandbox_" + sandboxID + "_allowed"
}

// BuildBaseScript produces the nft ruleset installed once at
// daemon startup. Contains:
//
//   - the table itself
//   - the forward chain, default policy drop, dispatching on
//     iifname to the appropriate per-sandbox chain
//   - the sandbox_chains verdict map used for that dispatch
//   - the postrouting chain with masquerade on the egress iface
//
// Rendered as a string so tests can assert it byte-for-byte, and
// so callers can inspect or log what got applied.
func BuildBaseScript(egressIface string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", NftTableName)
	fmt.Fprintf(&b, "\tmap %s {\n", nftSandboxMap)
	fmt.Fprintf(&b, "\t\ttype ifname : verdict\n")
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\tchain %s {\n", nftForwardChain)
	fmt.Fprintf(&b, "\t\ttype filter hook forward priority 0; policy drop;\n")
	// Allow return traffic from already-established connections.
	// Without this, reply packets for allowed outbound connections
	// would be dropped by the default policy on the return path.
	fmt.Fprintf(&b, "\t\tct state established,related accept\n")
	// Dispatch outbound by ingress iface name to per-sandbox chain.
	fmt.Fprintf(&b, "\t\tiifname vmap @%s\n", nftSandboxMap)
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\tchain %s {\n", nftPostroutingChain)
	fmt.Fprintf(&b, "\t\ttype nat hook postrouting priority 100;\n")
	fmt.Fprintf(&b, "\t\toifname %q masquerade\n", egressIface)
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "}\n")
	return b.String()
}

// BuildSandboxScript produces the per-sandbox nft objects:
//
//   - a timeout-flagged set of allowed IPv4 addresses
//   - a chain with the policy rules referencing that set
//   - an entry in the dispatch map pointing iifname → this chain
//
// The per-sandbox chain structure, in order:
//
//  1. accept DNS to the anycast IP (port 53 UDP only)
//  2. accept any packet whose destination is in the allowed set
//  3. fall through → drop (implicitly, because the forward chain's
//     default policy is drop and we don't accept here)
func BuildSandboxScript(sandboxID string, hostIface string, anycast netip.Addr) string {
	chain := sandboxChainName(sandboxID)
	set := sandboxAllowedSetName(sandboxID)

	var b strings.Builder
	// Set first — the chain references it.
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags timeout; }\n",
		NftTableName, set)
	// Chain.
	fmt.Fprintf(&b, "add chain inet %s %s\n", NftTableName, chain)
	// Rules inside the chain.
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr %s udp dport 53 accept\n",
		NftTableName, chain, anycast)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s accept\n",
		NftTableName, chain, set)
	// Map entry: iifname → jump chain. Quote the iifname so it
	// parses as an ifname literal.
	fmt.Fprintf(&b, "add element inet %s %s { %q : jump %s }\n",
		NftTableName, nftSandboxMap, hostIface, chain)
	return b.String()
}

// BuildSandboxTeardownScript removes everything BuildSandboxScript
// added. Used in Teardown and by the orphan reap.
//
// The map element is removed first so no new packets dispatch to
// the chain during the brief window where the chain is being
// emptied.
func BuildSandboxTeardownScript(sandboxID string, hostIface string) string {
	chain := sandboxChainName(sandboxID)
	set := sandboxAllowedSetName(sandboxID)

	var b strings.Builder
	fmt.Fprintf(&b, "delete element inet %s %s { %q }\n",
		NftTableName, nftSandboxMap, hostIface)
	fmt.Fprintf(&b, "delete chain inet %s %s\n", NftTableName, chain)
	fmt.Fprintf(&b, "delete set inet %s %s\n", NftTableName, set)
	return b.String()
}

// EnsureBaseTable installs the base ruleset. Safe to call on a
// system that already has the table — it re-creates it after
// tearing down any previous instance, guaranteeing a clean state
// on startup. Operators who want to preserve state across daemon
// restarts shouldn't; every restart is a fresh world.
func EnsureBaseTable(ctx context.Context, egressIface string) error {
	// Flush any prior state, ignoring "no such table" errors.
	if err := runCmd(ctx, "nft", "flush", "table", "inet", NftTableName); err != nil {
		if !isNoSuchTable(err) {
			return fmt.Errorf("flush existing table: %w", err)
		}
	}
	if err := runCmd(ctx, "nft", "delete", "table", "inet", NftTableName); err != nil {
		if !isNoSuchTable(err) {
			return fmt.Errorf("delete existing table: %w", err)
		}
	}
	script := BuildBaseScript(egressIface)
	if err := runCmdStdin(ctx, script, "nft", "-f", "-"); err != nil {
		return fmt.Errorf("install base table: %w", err)
	}
	return nil
}

// TeardownBaseTable removes the entire crucible table. Called by
// orphan reap at startup before EnsureBaseTable installs a fresh
// copy.
func TeardownBaseTable(ctx context.Context) error {
	if err := runCmd(ctx, "nft", "delete", "table", "inet", NftTableName); err != nil {
		if isNoSuchTable(err) {
			return nil
		}
		return err
	}
	return nil
}

// InstallSandbox applies BuildSandboxScript via nft -f -. Must be
// called after EnsureBaseTable and after the host-side veth
// (hostIface) exists.
func InstallSandbox(ctx context.Context, sandboxID, hostIface string, anycast netip.Addr) error {
	script := BuildSandboxScript(sandboxID, hostIface, anycast)
	return runCmdStdin(ctx, script, "nft", "-f", "-")
}

// RemoveSandbox applies BuildSandboxTeardownScript. Idempotent —
// missing objects are treated as success so double-teardown
// doesn't fail.
func RemoveSandbox(ctx context.Context, sandboxID, hostIface string) error {
	script := BuildSandboxTeardownScript(sandboxID, hostIface)
	if err := runCmdStdin(ctx, script, "nft", "-f", "-"); err != nil {
		// Partial-remove is common (e.g., the chain was already
		// deleted by a prior failed removal). Log at the caller;
		// here we just bucket "not found" errors into nil.
		if isNoSuchObject(err) {
			return nil
		}
		return err
	}
	return nil
}

// AllowIP adds an IPv4 address to the sandbox's allowed-IPs set
// with the given timeout. Called by the DNS proxy on each allowed
// resolution.
//
// Timeout is clamped to sensible bounds — 1 second is the nft
// minimum granularity, and anything longer than an hour is almost
// certainly a DNS record with a degenerate TTL (we cap to keep
// the set from growing unbounded).
func AllowIP(ctx context.Context, sandboxID string, ip netip.Addr, ttl time.Duration) error {
	if !ip.Is4() {
		return fmt.Errorf("network: AllowIP only supports IPv4, got %s", ip)
	}
	if ttl < time.Second {
		ttl = time.Second
	}
	if ttl > time.Hour {
		ttl = time.Hour
	}
	set := sandboxAllowedSetName(sandboxID)
	elem := fmt.Sprintf("add element inet %s %s { %s timeout %ds }\n",
		NftTableName, set, ip, int64(ttl.Seconds()))
	return runCmdStdin(ctx, elem, "nft", "-f", "-")
}

// isNoSuchTable recognizes nft's error message for a missing
// table. Matched by substring because nft doesn't surface a
// stable exit code for this case.
func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	return containsAny(err.Error(),
		"No such file or directory",
		"does not exist",
	)
}

// isNoSuchObject is the same pattern for missing sets/chains/map
// entries during teardown.
func isNoSuchObject(err error) bool {
	if err == nil {
		return false
	}
	return containsAny(err.Error(),
		"No such file or directory",
		"does not exist",
		"not exist",
	)
}
