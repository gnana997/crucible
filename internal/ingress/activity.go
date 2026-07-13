package ingress

import (
	"sync"
	"time"
)

// ActivityTracker records, per app, the last time a request was seen and how
// many are in flight. The proxy writes to it (begin/end around each forwarded
// request); the idle monitor reads it (Activity) to decide when an app has been
// idle long enough to sleep. Deliberately dumb: a timestamp and a counter, no
// rate modeling.
type ActivityTracker struct {
	mu   sync.Mutex
	now  func() time.Time
	apps map[string]*appActivity
}

type appActivity struct {
	last     time.Time
	inflight int
}

// NewActivityTracker returns an empty tracker.
func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{now: time.Now, apps: map[string]*appActivity{}}
}

func (t *ActivityTracker) get(name string) *appActivity {
	a := t.apps[name]
	if a == nil {
		a = &appActivity{}
		t.apps[name] = a
	}
	return a
}

// begin marks the start of a request for name: one more in flight, activity now.
func (t *ActivityTracker) begin(name string) {
	if name == "" {
		return
	}
	t.mu.Lock()
	a := t.get(name)
	a.inflight++
	a.last = t.now()
	t.mu.Unlock()
}

// end marks a request for name finished: one fewer in flight, activity now (so a
// long-running request's completion also counts as recent activity).
func (t *ActivityTracker) end(name string) {
	if name == "" {
		return
	}
	t.mu.Lock()
	a := t.get(name)
	if a.inflight > 0 {
		a.inflight--
	}
	a.last = t.now()
	t.mu.Unlock()
}

// Seen records activity for name without opening a request — it stamps last=now
// and creates the entry so Activity reports ok=true. The L4 waking forwarder
// calls it when it starts fronting a scale-to-zero app, so an app that has never
// had a connection still becomes idle-monitor-eligible and sleeps after its
// idle_timeout from readiness (rather than staying awake forever, unseen).
func (t *ActivityTracker) Seen(name string) {
	if name == "" {
		return
	}
	t.mu.Lock()
	t.get(name).last = t.now()
	t.mu.Unlock()
}

// Activity reports the last-seen time and in-flight count for name. ok is false
// when the app has never been seen through the proxy (so the idle monitor leaves
// it alone rather than sleeping an app it can't observe).
func (t *ActivityTracker) Activity(name string) (last time.Time, inflight int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.apps[name]
	if a == nil {
		return time.Time{}, 0, false
	}
	return a.last, a.inflight, true
}
