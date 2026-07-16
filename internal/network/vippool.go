package network

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"
)

// DefaultInternalVIPCIDR is the IPv4 block per-app internal VIPs (v0.9.5 app→app
// L4) are allocated from when --internal-vip-cidr is unset. It is deliberately a
// sibling of the default subnet pool (10.20.0.0/16) so the two never overlap out of
// the box; the daemon still validates non-overlap at startup (ValidateVIPCIDR).
//
// A VIP is a *virtual* address — like a Kubernetes ClusterIP it names an app, not a
// guest NIC. It lives host-local on the anycast dummy iface and is reached via nft.
var DefaultInternalVIPCIDR = netip.MustParsePrefix("10.21.0.0/16")

// ErrVIPPoolExhausted is returned by VIPPool.Acquire when every usable address in
// the pool's base CIDR is leased. Wrap with %w for errors.Is at call sites.
var ErrVIPPoolExhausted = errors.New("network: internal VIP pool exhausted")

// VIPPool allocates single IPv4 addresses (/32 VIPs) from a base CIDR, one per app.
// It mirrors subnet.Pool — a forward-sweeping cursor for debuggable non-recycled
// addresses, a free bitmap, the network/broadcast addresses of the base reserved —
// but hands out one address rather than a /30 block. Thread-safe.
type VIPPool struct {
	base     netip.Prefix
	baseAddr uint32
	total    int // number of addresses in base (2^(32-bits))
	reserved map[int]bool

	mu     sync.Mutex
	free   *bitmap
	cursor int
	inUse  int
}

// NewVIPPool constructs a VIP pool over base. The base's network address (.0) and
// broadcast address (last) are always reserved, as is the /32 of every addr in
// reserved (e.g. a well-known VIP). base must be IPv4 and /30 or larger (so at
// least two addresses remain usable after the net/broadcast reservations).
func NewVIPPool(base netip.Prefix, reserved ...netip.Addr) (*VIPPool, error) {
	if !base.IsValid() {
		return nil, errors.New("network: invalid VIP base prefix")
	}
	if !base.Addr().Is4() {
		return nil, errors.New("network: VIP base prefix must be IPv4")
	}
	if base.Bits() > 30 {
		return nil, fmt.Errorf("network: VIP base prefix %s too small (need /30 or larger)", base)
	}
	base = base.Masked()
	total := 1 << (32 - base.Bits())

	p := &VIPPool{
		base:     base,
		baseAddr: addrAsUint32(base.Addr()),
		total:    total,
		reserved: map[int]bool{},
		free:     newBitmap(total),
		cursor:   -1,
	}
	// Reserve the base's network (.0) and broadcast (last) addresses.
	p.reserve(0)
	p.reserve(total - 1)
	for _, r := range reserved {
		if !r.Is4() {
			return nil, fmt.Errorf("network: reserved VIP %s is not IPv4", r)
		}
		if !p.base.Contains(r) {
			return nil, fmt.Errorf("network: reserved VIP %s not inside base %s", r, p.base)
		}
		p.reserve(int(addrAsUint32(r) - p.baseAddr))
	}
	return p, nil
}

// reserve marks idx permanently taken. Must be called before the pool is shared.
func (p *VIPPool) reserve(idx int) {
	if idx < 0 || idx >= p.total || p.reserved[idx] {
		return
	}
	p.reserved[idx] = true
	p.free.set(idx)
	p.inUse++
}

// Acquire returns a fresh VIP or an error wrapping ErrVIPPoolExhausted.
func (p *VIPPool) Acquire() (netip.Addr, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	idx, ok := p.free.findFirstZeroFrom(p.cursor + 1)
	if !ok {
		idx, ok = p.free.findFirstZeroFrom(0) // wrap: releases may have freed earlier slots
	}
	if !ok {
		return netip.Addr{}, ErrVIPPoolExhausted
	}
	p.free.set(idx)
	p.cursor = idx
	p.inUse++
	return p.addrAt(idx), nil
}

// Release returns a VIP to the pool. Idempotent; a VIP that isn't in this pool, was
// already released, or is reserved is a no-op.
func (p *VIPPool) Release(addr netip.Addr) {
	if !addr.Is4() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.base.Contains(addr) {
		return
	}
	idx := int(addrAsUint32(addr) - p.baseAddr)
	if idx < 0 || idx >= p.total || p.reserved[idx] || !p.free.get(idx) {
		return
	}
	p.free.clear(idx)
	p.inUse--
}

// InUse returns the count of held VIPs (including reserved). Capacity is the total
// address count; InUse/Capacity tells operators how close the pool is to exhaustion.
func (p *VIPPool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inUse
}

// Capacity returns the total number of addresses in the pool, reserved included.
func (p *VIPPool) Capacity() int { return p.total }

func (p *VIPPool) addrAt(idx int) netip.Addr {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], p.baseAddr+uint32(idx))
	return netip.AddrFrom4(buf)
}

// ValidateVIPCIDR checks a configured --internal-vip-cidr is usable and does not
// overlap the guest subnet pool — the same non-overlap rule Kubernetes enforces
// between the service and pod CIDRs (an overlap would let a VIP collide with a live
// guest /30). Called at daemon startup.
func ValidateVIPCIDR(vipCIDR, subnetPool netip.Prefix) error {
	if !vipCIDR.IsValid() || !vipCIDR.Addr().Is4() {
		return fmt.Errorf("network: --internal-vip-cidr must be a valid IPv4 CIDR, got %q", vipCIDR)
	}
	if vipCIDR.Bits() > 30 {
		return fmt.Errorf("network: --internal-vip-cidr %s too small (need /30 or larger)", vipCIDR)
	}
	if subnetPool.IsValid() && vipCIDR.Overlaps(subnetPool) {
		return fmt.Errorf("network: --internal-vip-cidr %s overlaps --network-subnet-pool %s (must be disjoint)",
			vipCIDR.Masked(), subnetPool.Masked())
	}
	return nil
}

// AddInternalVIP assigns a per-app VIP as an additional /32 on the anycast dummy
// iface (crucibleDNSIface), so the ingress proxy can bind VIP:port. Idempotent: an
// address already present is treated as success. RemoveInternalVIP drops it.
func AddInternalVIP(ctx context.Context, addr netip.Addr) error {
	cidr := fmt.Sprintf("%s/32", addr)
	if err := runCmd(ctx, "ip", "addr", "add", cidr, "dev", crucibleDNSIface); err != nil {
		if isAddrAlreadyAssigned(ctx, err) {
			return nil // already assigned
		}
		return fmt.Errorf("network: add VIP %s to %s: %w", cidr, crucibleDNSIface, err)
	}
	return nil
}

// RemoveInternalVIP drops a per-app VIP from the anycast dummy iface. A missing
// address (or missing iface) is treated as success (idempotent).
func RemoveInternalVIP(ctx context.Context, addr netip.Addr) error {
	cidr := fmt.Sprintf("%s/32", addr)
	if err := runCmd(ctx, "ip", "addr", "del", cidr, "dev", crucibleDNSIface); err != nil {
		if isAddrNotAssigned(ctx, err) || isCannotFindDevice(ctx, err) {
			return nil
		}
		return fmt.Errorf("network: del VIP %s from %s: %w", cidr, crucibleDNSIface, err)
	}
	return nil
}

// isAddrAlreadyAssigned reports whether an `ip addr add` failed only because the
// address is already present ("RTNETLINK answers: File exists").
func isAddrAlreadyAssigned(ctx context.Context, err error) bool {
	return ranAndExitedNonZero(ctx, err) && containsAny(err.Error(), "File exists")
}

// isAddrNotAssigned reports whether an `ip addr del` failed only because the
// address was already gone.
func isAddrNotAssigned(ctx context.Context, err error) bool {
	return ranAndExitedNonZero(ctx, err) &&
		containsAny(err.Error(), "Cannot assign requested address", "does not exist")
}
