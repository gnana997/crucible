package ingress

import (
	"testing"
	"time"
)

func TestBalancerSingleInstance(t *testing.T) {
	b := NewBalancer()
	set := []Target{{InstanceID: "a", GuestIP: "10.0.0.1", Port: 80}}
	tg, release := b.Pick(set)
	if tg.InstanceID != "a" {
		t.Fatalf("single instance: picked %q, want a", tg.InstanceID)
	}
	release()
}

func TestBalancerLeastRequest(t *testing.T) {
	b := NewBalancer()
	b.randN = func(int) int { return 0 } // deterministic P2C sample: indices 0 and 1
	set := []Target{
		{InstanceID: "a", GuestIP: "10.0.0.1", Port: 80},
		{InstanceID: "b", GuestIP: "10.0.0.2", Port: 80},
	}
	// Warm both (past the slow-start window); a is loaded, b is idle.
	old := b.now().Add(-time.Minute)
	b.insts = map[string]*instLoad{
		"a": {inflight: 3, firstSeen: old},
		"b": {inflight: 0, firstSeen: old},
	}
	tg, release := b.Pick(set)
	if tg.InstanceID != "b" {
		t.Errorf("least-request: picked %q, want b (fewer in-flight)", tg.InstanceID)
	}
	release()
}

func TestBalancerEjectsFailingInstance(t *testing.T) {
	b := NewBalancer()
	set := []Target{
		{InstanceID: "a", GuestIP: "10.0.0.1", Port: 80},
		{InstanceID: "b", GuestIP: "10.0.0.2", Port: 80},
	}
	b.Pick(set) // register both

	for i := 0; i < balancerEjectFails; i++ {
		b.Fail("b")
	}
	got := map[string]int{}
	for i := 0; i < 20; i++ {
		tg, release := b.Pick(set)
		got[tg.InstanceID]++
		release()
	}
	if got["b"] != 0 {
		t.Errorf("ejected instance b was still picked %d times", got["b"])
	}
	if got["a"] == 0 {
		t.Error("instance a was never picked while b was ejected")
	}
}

func TestBalancerSlowStartDeprioritizesFresh(t *testing.T) {
	b := NewBalancer()
	now := b.now()
	// a is warm (old), b is brand new → effLoad(b) carries a slow-start penalty.
	b.insts = map[string]*instLoad{
		"a": {inflight: 1, firstSeen: now.Add(-time.Minute)},
		"b": {inflight: 0, firstSeen: now},
	}
	if la, lb := b.effLoad("a", now), b.effLoad("b", now); lb <= la {
		t.Errorf("slow-start: fresh b effLoad %d should exceed warm a effLoad %d", lb, la)
	}
}

func TestBalancerFailOpenWhenAllEjected(t *testing.T) {
	b := NewBalancer()
	set := []Target{
		{InstanceID: "a", GuestIP: "10.0.0.1", Port: 80},
		{InstanceID: "b", GuestIP: "10.0.0.2", Port: 80},
	}
	b.Pick(set) // register both
	for i := 0; i < balancerEjectFails; i++ {
		b.Fail("a")
		b.Fail("b")
	}
	// Both ejected → fail open: still returns one (better than dropping traffic).
	tg, release := b.Pick(set)
	if tg.InstanceID != "a" && tg.InstanceID != "b" {
		t.Errorf("all-ejected fail-open picked %q, want a or b", tg.InstanceID)
	}
	release()
}

func TestBalancerFailUnknownIsNoop(t *testing.T) {
	b := NewBalancer()
	b.Fail("")      // empty id
	b.Fail("ghost") // never registered
	tg, release := b.Pick([]Target{{InstanceID: "a", GuestIP: "10.0.0.1", Port: 80}})
	if tg.InstanceID != "a" {
		t.Errorf("picked %q, want a", tg.InstanceID)
	}
	release()
}
