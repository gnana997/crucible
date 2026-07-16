package ingress

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/gnana997/crucible/sdk/api"
)

// seqPool hands out 10.21.0.1, .2, ... and tracks releases.
type seqPool struct {
	next     int
	released []netip.Addr
	fail     bool
}

func (p *seqPool) Acquire() (netip.Addr, error) {
	if p.fail {
		return netip.Addr{}, errors.New("exhausted")
	}
	p.next++
	return netip.AddrFrom4([4]byte{10, 21, 0, byte(p.next)}), nil
}
func (p *seqPool) Release(a netip.Addr) { p.released = append(p.released, a) }

// recHooks records every hook call for assertions.
type recHooks struct {
	added, removed   []netip.Addr
	opened, closed   map[netip.Addr][]int
	bound            map[string]netip.Addr
	boundPorts       map[string][]L4Port
	unbound          []string
	bindErr, openErr error
}

func newRecHooks() *recHooks {
	return &recHooks{opened: map[netip.Addr][]int{}, closed: map[netip.Addr][]int{}, bound: map[string]netip.Addr{}, boundPorts: map[string][]L4Port{}}
}

func (h *recHooks) hooks() InternalL4Hooks {
	return InternalL4Hooks{
		AddVIP:     func(v netip.Addr) error { h.added = append(h.added, v); return nil },
		RemoveVIP:  func(v netip.Addr) error { h.removed = append(h.removed, v); return nil },
		OpenPorts:  func(v netip.Addr, p []int) error { h.opened[v] = p; return h.openErr },
		ClosePorts: func(v netip.Addr, p []int) error { h.closed[v] = p; return nil },
		Bind: func(app string, v netip.Addr, ports []L4Port) error {
			if h.bindErr != nil {
				return h.bindErr
			}
			h.bound[app] = v
			h.boundPorts[app] = ports
			return nil
		},
		Unbind: func(app string) { h.unbound = append(h.unbound, app) },
	}
}

func specWithPorts(name string, ports ...api.InternalPort) api.AppSpec {
	return api.AppSpec{Name: name, InternalPorts: ports}
}

func TestInternalL4SetExposeAndTeardown(t *testing.T) {
	pool := &seqPool{}
	h := newRecHooks()
	s := NewInternalL4Set(pool, h.hooks(), quietLog())

	// Expose db (5432 tcp) — VIP assigned, nft opened, proxy bound, DNS lookup works.
	s.Reconcile([]api.AppSpec{specWithPorts("db", api.InternalPort{Port: 5432})})
	vip, ok := s.VIPFor("db")
	if !ok || vip.String() != "10.21.0.1" {
		t.Fatalf("VIPFor(db) = %v,%v, want 10.21.0.1", vip, ok)
	}
	if len(h.added) != 1 || h.added[0] != vip {
		t.Fatalf("AddVIP not called with %v: %v", vip, h.added)
	}
	if got := h.opened[vip]; len(got) != 1 || got[0] != 5432 {
		t.Fatalf("OpenPorts = %v, want [5432]", got)
	}
	if h.bound["db"] != vip {
		t.Fatalf("Bind(db) vip = %v, want %v", h.bound["db"], vip)
	}

	// Reconcile with the same spec again → no-op (idempotent, no re-acquire).
	s.Reconcile([]api.AppSpec{specWithPorts("db", api.InternalPort{Port: 5432})})
	if pool.next != 1 {
		t.Fatalf("idempotent reconcile re-acquired a VIP (next=%d)", pool.next)
	}

	// Drop the app → full teardown + VIP released.
	s.Reconcile(nil)
	if _, ok := s.VIPFor("db"); ok {
		t.Fatalf("db still exposed after teardown")
	}
	if len(h.unbound) != 1 || h.unbound[0] != "db" {
		t.Fatalf("Unbind(db) not called: %v", h.unbound)
	}
	if len(h.removed) != 1 || h.removed[0] != vip {
		t.Fatalf("RemoveVIP not called: %v", h.removed)
	}
	if len(pool.released) != 1 || pool.released[0] != vip {
		t.Fatalf("VIP not released: %v", pool.released)
	}
}

func TestInternalL4SetPortChangeRebinds(t *testing.T) {
	pool := &seqPool{}
	h := newRecHooks()
	s := NewInternalL4Set(pool, h.hooks(), quietLog())

	s.Reconcile([]api.AppSpec{specWithPorts("db", api.InternalPort{Port: 5432})})
	v1, _ := s.VIPFor("db")

	// Change the ports → app is torn down and re-set-up (unbind + rebind).
	s.Reconcile([]api.AppSpec{specWithPorts("db", api.InternalPort{Port: 5432}, api.InternalPort{Port: 6432, Proto: "http"})})
	if len(h.unbound) != 1 {
		t.Fatalf("port change should unbind once, got %v", h.unbound)
	}
	got := h.boundPorts["db"]
	if len(got) != 2 || got[0].Port != 5432 || got[1].Port != 6432 || got[1].Proto != "http" {
		t.Fatalf("rebound ports = %+v, want [5432/tcp,6432/http] sorted", got)
	}
	// A fresh VIP was acquired for the re-setup (old released).
	if _, ok := s.VIPFor("db"); !ok {
		t.Fatalf("db not exposed after port change")
	}
	if len(pool.released) != 1 || pool.released[0] != v1 {
		t.Fatalf("old VIP not released on rebind: %v", pool.released)
	}
}

func TestInternalL4SetIgnoresAppsWithoutPorts(t *testing.T) {
	pool := &seqPool{}
	h := newRecHooks()
	s := NewInternalL4Set(pool, h.hooks(), quietLog())
	s.Reconcile([]api.AppSpec{{Name: "web"}}) // no InternalPorts
	if _, ok := s.VIPFor("web"); ok {
		t.Fatalf("app with no InternalPorts should not be exposed")
	}
	if pool.next != 0 {
		t.Fatalf("should not acquire a VIP for an app with no ports")
	}
}

func TestInternalL4SetBindFailureRollsBack(t *testing.T) {
	pool := &seqPool{}
	h := newRecHooks()
	h.bindErr = errors.New("bind boom")
	s := NewInternalL4Set(pool, h.hooks(), quietLog())

	s.Reconcile([]api.AppSpec{specWithPorts("db", api.InternalPort{Port: 5432})})
	if _, ok := s.VIPFor("db"); ok {
		t.Fatalf("app should not be exposed after Bind failure")
	}
	// Rollback: nft closed, VIP removed + released.
	if len(h.removed) != 1 || len(pool.released) != 1 {
		t.Fatalf("bind failure must roll back VIP: removed=%v released=%v", h.removed, pool.released)
	}
}

func TestInternalL4SetPoolExhaustion(t *testing.T) {
	pool := &seqPool{fail: true}
	h := newRecHooks()
	s := NewInternalL4Set(pool, h.hooks(), quietLog())
	s.Reconcile([]api.AppSpec{specWithPorts("db", api.InternalPort{Port: 5432})})
	if _, ok := s.VIPFor("db"); ok {
		t.Fatalf("no VIP available → app must not be exposed")
	}
	if len(h.added) != 0 {
		t.Fatalf("no side effects when pool is exhausted, got AddVIP %v", h.added)
	}
}
