package network

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
)

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", s, err)
	}
	return p
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse addr %q: %v", s, err)
	}
	return a
}

func TestNewPoolRejectsInvalidBase(t *testing.T) {
	cases := []struct {
		name string
		base netip.Prefix
	}{
		{"zero prefix", netip.Prefix{}},
		{"too small", mustPrefix(t, "10.20.0.0/31")},
		{"ipv6", mustPrefix(t, "fd00::/64")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPool(tc.base); err == nil {
				t.Fatalf("expected error for base %v", tc.base)
			}
		})
	}
}

func TestNewPoolAcceptsValidBases(t *testing.T) {
	for _, s := range []string{"10.20.0.0/30", "10.20.0.0/28", "10.20.0.0/24", "10.20.0.0/16"} {
		if _, err := NewPool(mustPrefix(t, s)); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

func TestPoolCapacityMatchesPrefix(t *testing.T) {
	cases := map[string]int{
		"10.20.0.0/30": 1,
		"10.20.0.0/28": 4,
		"10.20.0.0/24": 64,
		"10.20.0.0/16": 16384,
	}
	for s, want := range cases {
		p, err := NewPool(mustPrefix(t, s))
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if p.Capacity() != want {
			t.Errorf("%s capacity = %d, want %d", s, p.Capacity(), want)
		}
	}
}

func TestAcquireReturnsUniqueBlocks(t *testing.T) {
	p, err := NewPool(mustPrefix(t, "10.20.0.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[netip.Prefix]bool{}
	for i := 0; i < 64; i++ {
		l, err := p.Acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if seen[l.Prefix] {
			t.Fatalf("duplicate lease for %s", l.Prefix)
		}
		seen[l.Prefix] = true
	}
	if p.InUse() != 64 {
		t.Errorf("InUse = %d, want 64", p.InUse())
	}
}

func TestAcquireSkipsReserved(t *testing.T) {
	// /28 has 4 slots: .0/30, .4/30, .8/30, .12/30.
	// Reserve an address in the .8/30 block — it must never
	// appear in any lease.
	reserved := mustAddr(t, "10.20.0.10")
	p, err := NewPool(mustPrefix(t, "10.20.0.0/28"), reserved)
	if err != nil {
		t.Fatal(err)
	}
	if p.InUse() != 1 {
		t.Errorf("reserved should count as in-use: InUse = %d, want 1", p.InUse())
	}
	for i := 0; i < 3; i++ {
		l, err := p.Acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if l.Prefix.Contains(reserved) {
			t.Fatalf("lease %s contains reserved address %s", l.Prefix, reserved)
		}
	}
}

func TestAcquireExhaustionReturnsSentinel(t *testing.T) {
	p, err := NewPool(mustPrefix(t, "10.20.0.0/30"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err = p.Acquire()
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("err = %v, want ErrPoolExhausted", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	p, err := NewPool(mustPrefix(t, "10.20.0.0/30"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	p.Release(l)
	if p.InUse() != 0 {
		t.Errorf("InUse after release = %d, want 0", p.InUse())
	}
	if _, err := p.Acquire(); err != nil {
		t.Errorf("re-acquire after release: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	p, err := NewPool(mustPrefix(t, "10.20.0.0/30"))
	if err != nil {
		t.Fatal(err)
	}
	l, _ := p.Acquire()
	p.Release(l)
	p.Release(l) // second release is a no-op
	if p.InUse() != 0 {
		t.Errorf("double-release changed InUse: got %d", p.InUse())
	}
}

func TestReleaseIgnoresReserved(t *testing.T) {
	reserved := mustAddr(t, "10.20.0.2")
	p, err := NewPool(mustPrefix(t, "10.20.0.0/28"), reserved)
	if err != nil {
		t.Fatal(err)
	}
	// Forge a lease over the reserved block and try to release.
	forged := Lease{Prefix: mustPrefix(t, "10.20.0.0/30")}
	before := p.InUse()
	p.Release(forged)
	if p.InUse() != before {
		t.Errorf("Release on reserved block changed InUse: %d → %d", before, p.InUse())
	}
}

func TestLeaseGatewayAndGuestIPLayout(t *testing.T) {
	p, err := NewPool(mustPrefix(t, "10.20.7.0/30"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := p.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.Prefix.String(), "10.20.7.0/30"; got != want {
		t.Errorf("Prefix = %s, want %s", got, want)
	}
	if got, want := l.Gateway.String(), "10.20.7.1"; got != want {
		t.Errorf("Gateway = %s, want %s", got, want)
	}
	if got, want := l.GuestIP.String(), "10.20.7.3"; got != want {
		t.Errorf("GuestIP = %s, want %s", got, want)
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	// Stress under -race to flush out any mutex misuse.
	const goroutines = 16
	const iterations = 200

	p, err := NewPool(mustPrefix(t, "10.20.0.0/20")) // 1024 slots
	if err != nil {
		t.Fatal(err)
	}

	// Guard a shared set of currently-held prefixes; every
	// Acquire must see its lease absent, every Release must see
	// its lease present.
	var mu sync.Mutex
	held := map[netip.Prefix]bool{}

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*iterations)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				l, err := p.Acquire()
				if err != nil {
					errs <- fmt.Errorf("acquire: %w", err)
					return
				}
				mu.Lock()
				if held[l.Prefix] {
					mu.Unlock()
					errs <- fmt.Errorf("double-acquired %s", l.Prefix)
					return
				}
				held[l.Prefix] = true
				mu.Unlock()

				// Do some pretend work.
				_ = l.GuestIP.String()

				mu.Lock()
				delete(held, l.Prefix)
				mu.Unlock()
				p.Release(l)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if p.InUse() != 0 {
		t.Errorf("InUse after concurrent test = %d, want 0", p.InUse())
	}
}
