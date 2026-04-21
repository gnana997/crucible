package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"
)

// ErrPoolExhausted is returned by Pool.Acquire when every /30 in
// the pool's base CIDR is currently leased. Wrap with %w for
// errors.Is at call sites.
var ErrPoolExhausted = errors.New("network: subnet pool exhausted")

// Lease is one /30 handed out of the Pool. The three important
// addresses are pre-computed so callers don't duplicate the
// arithmetic everywhere:
//
//   - Prefix:  the whole /30, e.g. 10.20.7.0/30 (4 addresses total)
//   - Gateway: .1 — lives on the host-side veth, also the guest's
//     default gateway and its route target for the DNS anycast
//   - GuestIP: .3 — assigned to the guest's eth0 by DHCP
//
// The .0 address is the subnet identifier and .2 is reserved as
// the guest-side bridge address; neither is exposed on the Lease
// because callers don't need to reason about them individually.
type Lease struct {
	Prefix  netip.Prefix
	Gateway netip.Addr
	GuestIP netip.Addr
}

// Pool allocates non-overlapping /30 blocks from a base CIDR.
// Thread-safe: internal mutex guards the free bitmap, and the
// pool's published state is consistent across goroutines.
//
// The allocator uses a forward-sweeping cursor rather than reusing
// the lowest free index: two sandboxes created in quick succession
// get adjacent-but-distinct /30s rather than aggressively
// recycling IDs. Debuggers reading logs or packet captures benefit
// from "sandbox created 5 minutes ago != sandbox created now" at a
// glance.
type Pool struct {
	base     netip.Prefix
	total    int // number of /30 slots in base
	reserved map[int]bool

	mu     sync.Mutex
	free   *bitmap
	cursor int // last allocated index; search starts from cursor+1
	inUse  int
}

// NewPool constructs a Pool over base. Each address in reserved
// causes the /30 containing it to be permanently excluded from
// allocation — used for the DNS anycast and any future well-known
// host-root addresses within the subnet pool.
//
// base must be /30 or larger; anything smaller has no /30s to hand
// out. Pathological inputs (unspecified prefix, malformed CIDRs)
// are rejected.
func NewPool(base netip.Prefix, reserved ...netip.Addr) (*Pool, error) {
	if !base.IsValid() {
		return nil, errors.New("network: invalid base prefix")
	}
	if !base.Addr().Is4() {
		return nil, errors.New("network: base prefix must be IPv4 (IPv6 is a v0.2 target)")
	}
	if base.Bits() > 30 {
		return nil, fmt.Errorf("network: base prefix %s has no /30 blocks", base)
	}

	// Number of /30s = 2^(30 - base.Bits()).
	total := 1 << (30 - base.Bits())

	p := &Pool{
		base:     base.Masked(),
		total:    total,
		reserved: map[int]bool{},
		free:     newBitmap(total),
		cursor:   -1,
	}

	baseAddr := prefixBaseAsUint32(p.base)
	for _, r := range reserved {
		if !r.Is4() {
			return nil, fmt.Errorf("network: reserved %s is not IPv4", r)
		}
		if !p.base.Contains(r) {
			return nil, fmt.Errorf("network: reserved %s not inside base %s", r, p.base)
		}
		rU := addrAsUint32(r)
		idx := int((rU - baseAddr) >> 2) // >> 2 = divide by 4 (size of a /30)
		if !p.reserved[idx] {
			p.reserved[idx] = true
			p.free.set(idx) // mark as taken so the sweep skips it
			p.inUse++
		}
	}

	return p, nil
}

// Acquire returns a fresh Lease or an error wrapping
// ErrPoolExhausted. Forward-sweeping from the cursor.
func (p *Pool) Acquire() (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	idx, ok := p.free.findFirstZeroFrom(p.cursor + 1)
	if !ok {
		// Wrap-around search from 0, in case releases freed
		// earlier slots.
		idx, ok = p.free.findFirstZeroFrom(0)
	}
	if !ok {
		return Lease{}, ErrPoolExhausted
	}

	p.free.set(idx)
	p.cursor = idx
	p.inUse++
	return p.leaseAt(idx), nil
}

// Release returns a Lease to the pool. Idempotent — releasing a
// Lease that was already released, or one that doesn't belong to
// this pool, is a no-op. Reserved-at-construction slots cannot be
// released (doing so would silently corrupt invariants).
func (p *Pool) Release(l Lease) {
	if !l.Prefix.IsValid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	baseAddr := prefixBaseAsUint32(p.base)
	leaseAddr := prefixBaseAsUint32(l.Prefix)
	if leaseAddr < baseAddr {
		return
	}
	idx := int((leaseAddr - baseAddr) >> 2)
	if idx < 0 || idx >= p.total {
		return
	}
	if p.reserved[idx] {
		return
	}
	if !p.free.get(idx) {
		return // already released
	}
	p.free.clear(idx)
	p.inUse--
}

// InUse returns the count of currently-held leases (including
// reserved slots). Safe under concurrent Acquire/Release.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inUse
}

// Capacity returns the total number of /30 blocks in the pool,
// including reserved ones. InUse / Capacity tells operators how
// close they are to exhaustion.
func (p *Pool) Capacity() int {
	return p.total
}

// leaseAt builds a Lease for the /30 at the given slot index.
// Must hold p.mu.
func (p *Pool) leaseAt(idx int) Lease {
	blockBase := prefixBaseAsUint32(p.base) + uint32(idx)*4
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], blockBase)
	networkAddr := netip.AddrFrom4(buf)
	prefix := netip.PrefixFrom(networkAddr, 30)

	binary.BigEndian.PutUint32(buf[:], blockBase+1)
	gateway := netip.AddrFrom4(buf)

	binary.BigEndian.PutUint32(buf[:], blockBase+3)
	guest := netip.AddrFrom4(buf)

	return Lease{Prefix: prefix, Gateway: gateway, GuestIP: guest}
}

// --- small helpers keep the code above readable -----------------

// addrAsUint32 flattens a 4-byte IPv4 address into its host-order
// uint32 form. Panics if the address isn't Is4; callers should
// have validated.
func addrAsUint32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

// prefixBaseAsUint32 returns the .0 address of the prefix's /30
// alignment. Prefix.Masked().Addr() is documented to zero the host
// bits, so this is safe.
func prefixBaseAsUint32(p netip.Prefix) uint32 {
	return addrAsUint32(p.Masked().Addr())
}

// Bitmap helpers live in bitmap.go — standalone so the next
// consumer can share them without pulling in the subnet allocator.
