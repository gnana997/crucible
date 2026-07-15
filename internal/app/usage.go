package app

import (
	"log/slog"
	"sync"
	"time"
)

// Usage is an app's durable, cumulative usage counters — a per-app ledger the
// daemon persists so the numbers survive a daemon restart. Everything is
// monotonic and stored in fine integer sub-units (vCPU·milliseconds, MiB·
// milliseconds, request counts): lossless, so a reader takes deltas between
// reads and converts to vCPU-hours / MiB-hours / GiB-hours itself. This is an
// observability/durability feature — persistent usage metrics — not a billing
// engine; any rating/aggregation lives outside the daemon.
type Usage struct {
	AppID   string `json:"app_id"`
	AppName string `json:"app_name"`

	// ComputeVCPUMillis is Σ (vCPUs × milliseconds the app was awake). A slept
	// or stopped app accrues nothing — only a live VMM burns compute.
	ComputeVCPUMillis int64 `json:"compute_vcpu_millis"`
	// MemoryMiBMillis is Σ (MemoryMiB × milliseconds awake), same awake window.
	MemoryMiBMillis int64 `json:"memory_mib_millis"`
	// StorageMiBMillis is Σ (volume MiB × milliseconds the volume existed) —
	// accrued whether the app is awake OR asleep, because a slept app still
	// occupies its disk. Zero for an app with no volumes.
	StorageMiBMillis int64 `json:"storage_mib_millis"`

	// Requests is the total ingress-proxy requests routed to the app;
	// RequestsByCode splits it by HTTP status class ("2xx", "4xx", …).
	Requests       uint64            `json:"requests"`
	RequestsByCode map[string]uint64 `json:"requests_by_code,omitempty"`

	// UpdatedAt is the last time the record was flushed to the store.
	UpdatedAt time.Time `json:"updated_at"`
	// FinalizedAt, when set, means the app was deleted: this is its final usage,
	// retained so a control plane can still read it after the app is gone.
	FinalizedAt *time.Time `json:"finalized_at,omitempty"`
}

// usageAccum is the in-memory accrual state for one app: the cumulative counters
// (mirrored to the store) plus the *state in effect since lastTs*. Every accrual
// integrates the interval [lastTs, now] using the state currently held here,
// THEN the caller overwrites the state — so a change only affects time after it.
type usageAccum struct {
	u        Usage
	awake    bool
	vcpus    int
	memMiB   int
	volBytes int64
	lastTs   time.Time // zero until the first observe — first observe never back-fills
	dirty    bool
}

// usageLedger is the persistent-usage-metrics accrual engine. It is driven by
// the Manager: exact-boundary observes at sleep/wake (so "compute freezes while
// asleep" is precise) and a periodic tick that re-asserts each app's current
// state (self-correcting for transitions not explicitly hooked). All methods are
// safe for concurrent use.
type usageLedger struct {
	store *Store
	now   func() time.Time
	log   *slog.Logger

	mu    sync.Mutex
	accum map[string]*usageAccum // by app id
}

func newUsageLedger(store *Store, now func() time.Time, log *slog.Logger) *usageLedger {
	if log == nil {
		log = slog.Default()
	}
	return &usageLedger{store: store, now: now, log: log, accum: make(map[string]*usageAccum)}
}

// get returns the accum for id, lazily loading its persisted counters on first
// touch (lastTs stays zero, so the load never back-fills the elapsed time).
func (l *usageLedger) get(id, name string) *usageAccum {
	a := l.accum[id]
	if a == nil {
		a = &usageAccum{}
		if u, found, err := l.store.GetUsage(id); err == nil && found {
			a.u = u
		} else {
			a.u = Usage{AppID: id}
		}
		l.accum[id] = a
	}
	if a.u.AppID == "" {
		a.u.AppID = id
	}
	if name != "" {
		a.u.AppName = name
	}
	if a.u.RequestsByCode == nil {
		a.u.RequestsByCode = make(map[string]uint64)
	}
	return a
}

// accrue integrates [lastTs, now] into the cumulative counters using the state
// currently held in a, then advances lastTs. Time-based dimensions accrue in
// whole milliseconds (sub-ms remainders are dropped — negligible at tick scale).
func (l *usageLedger) accrue(a *usageAccum, now time.Time) {
	if !a.lastTs.IsZero() {
		if dtMs := now.Sub(a.lastTs).Milliseconds(); dtMs > 0 {
			if a.awake {
				a.u.ComputeVCPUMillis += int64(a.vcpus) * dtMs
				a.u.MemoryMiBMillis += int64(a.memMiB) * dtMs
			}
			if a.volBytes > 0 {
				a.u.StorageMiBMillis += (a.volBytes >> 20) * dtMs
			}
			a.dirty = true
		}
	}
	a.lastTs = now
}

// observe accrues the interval since the last observe under the app's PRIOR
// state, then records the new state and persists. Called both on lifecycle
// events (exact boundaries) and on the periodic tick (current state re-asserted,
// which simply accrues the elapsed interval).
func (l *usageLedger) observe(id, name string, awake bool, vcpus, memMiB int, volBytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.get(id, name)
	l.accrue(a, l.now())
	a.awake, a.vcpus, a.memMiB, a.volBytes = awake, vcpus, memMiB, volBytes
	l.flush(a)
}

// AddRequest bumps the durable request counters in memory (hot path — no store
// write; the next tick/observe flushes them, bounding loss to one interval).
func (l *usageLedger) AddRequest(id, name, code string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.get(id, name)
	a.u.Requests++
	a.u.RequestsByCode[code]++
	a.dirty = true
}

// Finalize records the final accrual for a deleted app, marks it finalized, and
// drops it from memory. The store record is RETAINED so a control plane can read
// the app's final usage after it's gone.
func (l *usageLedger) Finalize(id, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.get(id, name)
	l.accrue(a, l.now())
	t := l.now()
	a.u.FinalizedAt = &t
	a.dirty = true
	l.flush(a)
	delete(l.accum, id)
}

// Snapshot accrues up to now and returns a copy of the app's current usage — the
// read path (also used by tests to assert accrual at an exact clock value).
func (l *usageLedger) Snapshot(id, name string) Usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.get(id, name)
	l.accrue(a, l.now())
	return cloneUsage(a.u)
}

// flush persists a dirty accum to the store (best-effort: a store error is
// logged, not returned — a metrics write must not break a lifecycle transition).
func (l *usageLedger) flush(a *usageAccum) {
	if !a.dirty {
		return
	}
	a.u.UpdatedAt = l.now()
	if err := l.store.PutUsage(a.u.AppID, a.u); err != nil {
		l.log.Warn("persist usage failed", "app", a.u.AppName, "err", err)
		return
	}
	a.dirty = false
}

func cloneUsage(u Usage) Usage {
	c := u
	if u.RequestsByCode != nil {
		c.RequestsByCode = make(map[string]uint64, len(u.RequestsByCode))
		for k, v := range u.RequestsByCode {
			c.RequestsByCode[k] = v
		}
	}
	if u.FinalizedAt != nil {
		t := *u.FinalizedAt
		c.FinalizedAt = &t
	}
	return c
}
