package network

import (
	"errors"
	"net/netip"
	"testing"
)

func TestVIPPoolAcquireReleaseCycle(t *testing.T) {
	p, err := NewVIPPool(mustPrefix(t, "10.21.0.0/24"))
	if err != nil {
		t.Fatalf("NewVIPPool: %v", err)
	}
	// /24 = 256 addrs; .0 (network) and .255 (broadcast) reserved → 254 usable.
	if got := p.Capacity(); got != 256 {
		t.Fatalf("Capacity = %d, want 256", got)
	}
	if got := p.InUse(); got != 2 {
		t.Fatalf("InUse after construction = %d, want 2 (net+broadcast reserved)", got)
	}

	a, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// First usable address is .1 (index 1), not .0 (reserved network addr).
	if a.String() != "10.21.0.1" {
		t.Fatalf("first VIP = %s, want 10.21.0.1", a)
	}
	if !mustPrefix(t, "10.21.0.0/24").Contains(a) {
		t.Fatalf("VIP %s not inside base", a)
	}

	b, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	if a == b {
		t.Fatalf("Acquire returned the same VIP twice: %s", a)
	}

	p.Release(a)
	// Forward-sweeping cursor: after releasing .1, the next Acquire keeps sweeping
	// forward (does not immediately recycle .1) until it wraps.
	c, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire 3: %v", err)
	}
	if c == b {
		t.Fatalf("Acquire returned an in-use VIP: %s", c)
	}
}

func TestVIPPoolNoDuplicatesUnderExhaustion(t *testing.T) {
	// /29 = 8 addrs; .0 and .7 reserved → 6 usable.
	p, err := NewVIPPool(mustPrefix(t, "10.21.0.0/29"))
	if err != nil {
		t.Fatalf("NewVIPPool: %v", err)
	}
	seen := map[netip.Addr]bool{}
	for i := 0; i < 6; i++ {
		a, err := p.Acquire()
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if seen[a] {
			t.Fatalf("duplicate VIP handed out: %s", a)
		}
		seen[a] = true
		if a.String() == "10.21.0.0" || a.String() == "10.21.0.7" {
			t.Fatalf("handed out a reserved network/broadcast addr: %s", a)
		}
	}
	if _, err := p.Acquire(); !errors.Is(err, ErrVIPPoolExhausted) {
		t.Fatalf("7th Acquire err = %v, want ErrVIPPoolExhausted", err)
	}
	// Releasing one frees exactly one slot.
	for a := range seen {
		p.Release(a)
		break
	}
	if _, err := p.Acquire(); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if _, err := p.Acquire(); !errors.Is(err, ErrVIPPoolExhausted) {
		t.Fatalf("Acquire past capacity again err = %v, want exhausted", err)
	}
}

func TestVIPPoolReservedAddrs(t *testing.T) {
	res := netip.MustParseAddr("10.21.0.5")
	p, err := NewVIPPool(mustPrefix(t, "10.21.0.0/24"), res)
	if err != nil {
		t.Fatalf("NewVIPPool: %v", err)
	}
	for i := 0; i < 253; i++ { // 256 - net - broadcast - one reserved
		a, err := p.Acquire()
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if a == res {
			t.Fatalf("handed out the reserved VIP %s", res)
		}
	}
	if _, err := p.Acquire(); !errors.Is(err, ErrVIPPoolExhausted) {
		t.Fatalf("pool should be exhausted with the reserved addr excluded, got %v", err)
	}
}

func TestVIPPoolReleaseIdempotentAndForeign(t *testing.T) {
	p, _ := NewVIPPool(mustPrefix(t, "10.21.0.0/24"))
	a, _ := p.Acquire()
	before := p.InUse()
	p.Release(a)
	p.Release(a)                                // double release: no-op
	p.Release(netip.MustParseAddr("10.99.0.1")) // foreign addr: no-op
	p.Release(netip.MustParseAddr("10.21.0.0")) // reserved network: no-op
	if got := p.InUse(); got != before-1 {
		t.Fatalf("InUse = %d, want %d (only the one real release counts)", got, before-1)
	}
}

func TestNewVIPPoolRejectsBadBase(t *testing.T) {
	for _, s := range []string{"10.21.0.0/31", "::1/64"} {
		if _, err := NewVIPPool(mustPrefix(t, s)); err == nil {
			t.Fatalf("NewVIPPool(%s) should have failed", s)
		}
	}
}

func TestValidateVIPCIDR(t *testing.T) {
	sub := mustPrefix(t, "10.20.0.0/16")
	// Disjoint: OK.
	if err := ValidateVIPCIDR(mustPrefix(t, "10.21.0.0/16"), sub); err != nil {
		t.Fatalf("disjoint CIDRs should validate: %v", err)
	}
	// Overlapping (identical) → rejected.
	if err := ValidateVIPCIDR(mustPrefix(t, "10.20.0.0/16"), sub); err == nil {
		t.Fatalf("overlapping CIDRs must be rejected")
	}
	// Overlapping (VIP subset of subnet pool) → rejected.
	if err := ValidateVIPCIDR(mustPrefix(t, "10.20.5.0/24"), sub); err == nil {
		t.Fatalf("VIP CIDR inside the subnet pool must be rejected")
	}
	// Too small → rejected.
	if err := ValidateVIPCIDR(mustPrefix(t, "10.21.0.0/31"), sub); err == nil {
		t.Fatalf("too-small VIP CIDR must be rejected")
	}
	// IPv6 → rejected.
	if err := ValidateVIPCIDR(mustPrefix(t, "fd00::/48"), sub); err == nil {
		t.Fatalf("IPv6 VIP CIDR must be rejected")
	}
}

func TestVIPPoolLargeCapacity(t *testing.T) {
	// The documented default: a /16 gives 65536 addresses (65534 usable).
	p, err := NewVIPPool(DefaultInternalVIPCIDR)
	if err != nil {
		t.Fatalf("NewVIPPool(default): %v", err)
	}
	if p.Capacity() != 65536 {
		t.Fatalf("Capacity = %d, want 65536", p.Capacity())
	}
}
