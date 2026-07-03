package network

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/gnana997/crucible/internal/network/dnsproxy"
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
	nftInputChain       = "input"
	nftPostroutingChain = "postrouting"
	nftSandboxMap       = "sandbox_chains" // iifname verdict map
	nftGuestSourcesSet  = "guest_sources"  // ifname . guest-IP pairs
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
//   - the guest_sources set + input chain gating guest→host
//     traffic (only DNS to the anycast, only from a registered
//     (veth, guest IP) pair)
//   - the postrouting chain with masquerade on the egress iface
//
// Rendered as a string so tests can assert it byte-for-byte, and
// so callers can inspect or log what got applied.
func BuildBaseScript(egressIface string, dnsAnycast netip.Addr) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", NftTableName)
	fmt.Fprintf(&b, "\tmap %s {\n", nftSandboxMap)
	fmt.Fprintf(&b, "\t\ttype ifname : verdict\n")
	fmt.Fprintf(&b, "\t}\n")
	// guest_sources pairs each sandbox's host-side veth with its
	// guest IP. The input chain's DNS accept matches against it,
	// which is what makes the DNS proxy's source-IP policy lookup
	// kernel-attested: a guest spoofing another sandbox's IP sends
	// a (veth, saddr) pair that isn't in the set and is dropped.
	fmt.Fprintf(&b, "\tset %s {\n", nftGuestSourcesSet)
	fmt.Fprintf(&b, "\t\ttype ifname . ipv4_addr\n")
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
	// Input chain: the forward chain never sees guest packets whose
	// destination is host-local (the DNS anycast, veth gateways, any
	// daemon bound to 0.0.0.0) — those traverse INPUT. Policy stays
	// accept so the host's own traffic (SSH etc.) is untouched; we
	// gate only packets arriving from sandbox veths, allowing DNS to
	// the anycast and dropping everything else.
	fmt.Fprintf(&b, "\tchain %s {\n", nftInputChain)
	fmt.Fprintf(&b, "\t\ttype filter hook input priority 0; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname . ip saddr @%s ip daddr %s udp dport 53 accept\n",
		nftGuestSourcesSet, dnsAnycast)
	fmt.Fprintf(&b, "\t\tiifname %q drop\n", vethHostPrefix+"*")
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
//
// It also registers the (host veth, guest IP) pair in the base
// guest_sources set so the input chain accepts this guest's DNS.
func BuildSandboxScript(sandboxID string, hostIface string, guestIP, anycast netip.Addr) string {
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
	// Anti-spoof pair for the input chain's DNS accept.
	fmt.Fprintf(&b, "add element inet %s %s { %q . %s }\n",
		NftTableName, nftGuestSourcesSet, hostIface, guestIP)
	return b.String()
}

// BuildSandboxTeardownScript removes everything BuildSandboxScript
// added. Used in Teardown and by the orphan reap.
//
// The guest_sources pair and map element are removed first so no
// new packets are accepted or dispatched to the chain during the
// brief window where the chain is being emptied.
func BuildSandboxTeardownScript(sandboxID string, hostIface string, guestIP netip.Addr) string {
	chain := sandboxChainName(sandboxID)
	set := sandboxAllowedSetName(sandboxID)

	var b strings.Builder
	fmt.Fprintf(&b, "delete element inet %s %s { %q . %s }\n",
		NftTableName, nftGuestSourcesSet, hostIface, guestIP)
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
func EnsureBaseTable(ctx context.Context, egressIface string, dnsAnycast netip.Addr) error {
	// Flush any prior state, ignoring "no such table" errors.
	if err := runCmd(ctx, "nft", "flush", "table", "inet", NftTableName); err != nil {
		if !isNoSuchObject(err) {
			return fmt.Errorf("flush existing table: %w", err)
		}
	}
	if err := runCmd(ctx, "nft", "delete", "table", "inet", NftTableName); err != nil {
		if !isNoSuchObject(err) {
			return fmt.Errorf("delete existing table: %w", err)
		}
	}
	script := BuildBaseScript(egressIface, dnsAnycast)
	if err := runCmdStdin(ctx, script, "nft", "-f", "-"); err != nil {
		return fmt.Errorf("install base table: %w", err)
	}

	// Install iptables FORWARD ACCEPT rules for our veth pattern.
	// Docker (among others) sets `iptables -P FORWARD DROP` to
	// isolate its own bridges; that policy ALSO drops our masqueraded
	// sandbox traffic because netfilter evaluates every registered
	// chain at a given hook — an ACCEPT in our nft chain doesn't
	// override a DROP policy in iptables' chain. Explicit ACCEPTs
	// scoped to vh-+ (our veth prefix) let our traffic through
	// without touching the host's unrelated FORWARD rules.
	if err := ensureIptablesForward(ctx); err != nil {
		// Best-effort roll-back of our nft table so we don't leave
		// half-state behind.
		_ = runCmd(context.Background(), "nft", "delete", "table", "inet", NftTableName)
		return fmt.Errorf("install iptables forward accept: %w", err)
	}
	return nil
}

// TeardownBaseTable removes the entire crucible table. Called by
// orphan reap at startup before EnsureBaseTable installs a fresh
// copy.
func TeardownBaseTable(ctx context.Context) error {
	// Remove iptables ACCEPTs first; best-effort (ignore missing).
	_ = removeIptablesForward(ctx)

	if err := runCmd(ctx, "nft", "delete", "table", "inet", NftTableName); err != nil {
		if isNoSuchObject(err) {
			return nil
		}
		return err
	}
	return nil
}

// iptablesForwardComment tags our rules so we can find and remove
// them idempotently. A distinctive string keeps us out of the way
// of any hand-authored iptables rules.
const iptablesForwardComment = "crucible-accept-veth"

// ensureIptablesForward installs `-i vh-+ -j ACCEPT` and
// `-o vh-+ -j ACCEPT` at the top of the FORWARD chain. Idempotent:
// removes any prior copies first so repeated daemon starts don't
// accumulate duplicates.
//
// Uses the wildcard interface match `vh-+` (iptables' syntax for
// "any interface whose name starts with vh-") so we don't have to
// rewrite rules per sandbox.
func ensureIptablesForward(ctx context.Context) error {
	_ = removeIptablesForward(ctx)
	for _, dir := range []string{"-i", "-o"} {
		if err := runCmd(ctx, "iptables", "-I", "FORWARD", "1",
			dir, vethHostPrefix+"+", "-m", "comment", "--comment", iptablesForwardComment,
			"-j", "ACCEPT"); err != nil {
			return fmt.Errorf("iptables -I FORWARD %s: %w", dir, err)
		}
	}
	return nil
}

// removeIptablesForward deletes any FORWARD rules carrying our
// comment. Loops until no more match so repeated daemon runs that
// accumulated duplicates all get cleaned up.
func removeIptablesForward(ctx context.Context) error {
	for _, dir := range []string{"-i", "-o"} {
		for {
			err := runCmd(ctx, "iptables", "-D", "FORWARD",
				dir, vethHostPrefix+"+", "-m", "comment", "--comment", iptablesForwardComment,
				"-j", "ACCEPT")
			if err != nil {
				break
			}
		}
	}
	return nil
}

// InstallSandbox applies BuildSandboxScript via nft -f -. Must be
// called after EnsureBaseTable and after the host-side veth
// (hostIface) exists.
func InstallSandbox(ctx context.Context, sandboxID, hostIface string, guestIP, anycast netip.Addr) error {
	script := BuildSandboxScript(sandboxID, hostIface, guestIP, anycast)
	return runCmdStdin(ctx, script, "nft", "-f", "-")
}

// RemoveSandbox applies BuildSandboxTeardownScript. Idempotent —
// missing objects are treated as success so double-teardown
// doesn't fail.
func RemoveSandbox(ctx context.Context, sandboxID, hostIface string, guestIP netip.Addr) error {
	script := BuildSandboxTeardownScript(sandboxID, hostIface, guestIP)
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

// AllowIPs adds a batch of resolved IPv4 addresses to the
// sandbox's allowed-IPs set, one nft invocation per batch. Called
// by the DNS proxy once per allowed resolution reply — batching
// (rather than one fork per record) is what keeps a fat DNS
// response from forking thousands of nft processes.
//
// Each timeout is clamped to sensible bounds — 1 second is the nft
// minimum granularity, and anything longer than an hour is almost
// certainly a DNS record with a degenerate TTL (we cap to keep
// the set from growing unbounded).
//
// Addresses that aren't public global-unicast are refused outright.
// The proxy range-filters before calling; this re-check shares the
// exact same predicate (dnsproxy.IsPublicUnicast) so no future caller
// can open egress to link-local (cloud metadata), loopback, CGNAT
// (Alibaba metadata 100.100.100.200), private/sandbox-pool, or any
// other IANA special-purpose range.
func AllowIPs(ctx context.Context, sandboxID string, ips []dnsproxy.AllowedIP) error {
	if len(ips) == 0 {
		return nil
	}
	elems := make([]string, 0, len(ips))
	for _, e := range ips {
		if !e.Addr.Is4() {
			return fmt.Errorf("network: AllowIPs only supports IPv4, got %s", e.Addr)
		}
		if !dnsproxy.IsPublicUnicast(e.Addr) {
			return fmt.Errorf("network: AllowIPs refuses non-public address %s", e.Addr)
		}
		ttl := e.TTL
		if ttl < time.Second {
			ttl = time.Second
		}
		if ttl > time.Hour {
			ttl = time.Hour
		}
		elems = append(elems, fmt.Sprintf("%s timeout %ds", e.Addr, int64(ttl.Seconds())))
	}
	set := sandboxAllowedSetName(sandboxID)
	script := fmt.Sprintf("add element inet %s %s { %s }\n",
		NftTableName, set, strings.Join(elems, ", "))
	return runCmdStdin(ctx, script, "nft", "-f", "-")
}

// isNoSuchObject recognizes nft's "object already gone" result for a
// missing table/set/chain/element during teardown — the only failure we
// treat as success for idempotency. nft returns a bare exit 1 for every
// error, so we still key on its stderr phrase, but only after confirming
// the command actually ran and exited (ranAndExitedNonZero) — otherwise a
// context timeout or netlink failure could be misread as "already gone"
// and leak a live chain.
func isNoSuchObject(err error) bool {
	return ranAndExitedNonZero(err) &&
		containsAny(err.Error(),
			"No such file or directory",
			"does not exist",
		)
}
