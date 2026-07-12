package ingress

import (
	"math/rand"
	"sync"
	"time"
)

// Load-balancing tuning.
const (
	balancerSlowStart  = 10 * time.Second // ramp a freshly-seen instance's weight
	balancerSlowSteps  = 8                // initial slow-start penalty (effective extra load)
	balancerEjectFails = 5                // consecutive failures before ejection
	balancerEjectFor   = 15 * time.Second // ejection cooldown before re-admission
	balancerFailDecay  = 30 * time.Second // reset the failure count after a quiet spell
)

// Balancer picks an instance from an app's endpoint set with power-of-two-choices
// least-request selection. It tracks per-instance in-flight load, slow-starts a
// freshly-seen instance (so a just-forked replica with a cold cache isn't slammed
// on request one), and passively ejects an instance that keeps failing,
// re-admitting it after a cooldown. Instance ids are globally unique, so one flat
// map keys every app's instances. Concurrency-safe.
type Balancer struct {
	mu    sync.Mutex
	insts map[string]*instLoad // key: instance id
	now   func() time.Time
	randN func(n int) int
}

type instLoad struct {
	inflight     int
	firstSeen    time.Time
	consecFail   int
	lastFail     time.Time
	ejectedUntil time.Time
}

// NewBalancer builds an empty balancer.
func NewBalancer() *Balancer {
	return &Balancer{insts: map[string]*instLoad{}, now: time.Now, randN: rand.Intn}
}

// Pick chooses an instance from set (len >= 1) and returns it plus a release
// func that MUST be called when the request completes — it decrements the
// instance's in-flight count.
func (b *Balancer) Pick(set []Target) (Target, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()

	// Track every instance in the set; prune entries no longer present.
	present := make(map[string]bool, len(set))
	for _, t := range set {
		present[t.InstanceID] = true
		if _, ok := b.insts[t.InstanceID]; !ok {
			b.insts[t.InstanceID] = &instLoad{firstSeen: now}
		}
	}
	for id := range b.insts {
		if !present[id] {
			delete(b.insts, id)
		}
	}

	// Eligible = not currently ejected; fail open if all are ejected.
	var eligible []Target
	for _, t := range set {
		if now.Before(b.insts[t.InstanceID].ejectedUntil) {
			continue
		}
		eligible = append(eligible, t)
	}
	if len(eligible) == 0 {
		eligible = set
	}

	var chosen Target
	if len(eligible) == 1 {
		chosen = eligible[0]
	} else {
		// Power of two choices: sample two distinct, take the lower effective load.
		i := b.randN(len(eligible))
		j := b.randN(len(eligible) - 1)
		if j >= i {
			j++
		}
		if b.effLoad(eligible[i].InstanceID, now) <= b.effLoad(eligible[j].InstanceID, now) {
			chosen = eligible[i]
		} else {
			chosen = eligible[j]
		}
	}

	b.insts[chosen.InstanceID].inflight++
	id := chosen.InstanceID
	return chosen, func() {
		b.mu.Lock()
		if s, ok := b.insts[id]; ok && s.inflight > 0 {
			s.inflight--
		}
		b.mu.Unlock()
	}
}

// effLoad is in-flight plus a slow-start penalty that decays to 0 over the ramp
// window, so a freshly-seen instance is deprioritized until it's warm. Caller
// holds mu.
func (b *Balancer) effLoad(id string, now time.Time) int {
	st := b.insts[id]
	load := st.inflight
	if age := now.Sub(st.firstSeen); age < balancerSlowStart {
		load += balancerSlowSteps - int(age/(balancerSlowStart/balancerSlowSteps))
	}
	return load
}

// Fail records an upstream failure for an instance (from the proxy's error
// handler). Enough consecutive failures ejects it for a cooldown.
func (b *Balancer) Fail(id string) {
	if id == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.insts[id]
	if !ok {
		return
	}
	now := b.now()
	if now.Sub(st.lastFail) > balancerFailDecay {
		st.consecFail = 0
	}
	st.consecFail++
	st.lastFail = now
	if st.consecFail >= balancerEjectFails {
		st.ejectedUntil = now.Add(balancerEjectFor)
		st.consecFail = 0
	}
}
