package app

import (
	"errors"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

func TestValidateSpecSleepPolicy(t *testing.T) {
	base := func(sp *api.SleepPolicy) api.AppSpec {
		s := nginxSpec("web", wire.RestartAlways)
		s.Sleep = sp
		return s
	}
	cases := []struct {
		name string
		sp   *api.SleepPolicy
		ok   bool
	}{
		{"nil disables sleep", nil, true},
		{"scale-to-zero", &api.SleepPolicy{IdleTimeoutSec: 30, MinScale: 0}, true},
		{"keep one warm", &api.SleepPolicy{IdleTimeoutSec: 0, MinScale: 1}, true},
		{"negative idle", &api.SleepPolicy{IdleTimeoutSec: -1}, false},
		{"min_scale too high", &api.SleepPolicy{MinScale: 2}, false},
		{"negative min_scale", &api.SleepPolicy{MinScale: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpec(base(tc.sp))
			if tc.ok && err != nil {
				t.Fatalf("validateSpec: unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("validateSpec: expected error, got nil")
			}
		})
	}
}

// TestAppSleepWakeLifecycle drives the full app-level Sleep/Wake state machine
// against the fake instantiator: boot → sleep (phase asleep, counted) →
// reconcile-leaves-alone → wake (phase running), plus the wrong-state errors.
func TestAppSleepWakeLifecycle(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)

	mustCreate(t, m, nginxSpec("web", wire.RestartAlways), true)
	m.reconcile(ctx())
	if got, _ := m.GetByName("web"); got.Status == nil || got.Status.Phase != "running" {
		t.Fatalf("phase after boot = %v, want running", got.Status)
	}

	// Can't wake a running app.
	if err := m.Wake(ctx(), "web"); !errors.Is(err, ErrNotAsleep) {
		t.Fatalf("Wake(running) err = %v, want ErrNotAsleep", err)
	}

	// Sleep.
	if err := m.Sleep(ctx(), "web"); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	got, _ := m.GetByName("web")
	if got.Status.Phase != "asleep" || got.Status.SleepCount != 1 {
		t.Fatalf("after sleep: phase=%q sleep_count=%d, want asleep/1", got.Status.Phase, got.Status.SleepCount)
	}

	// The reconciler must leave the slept app alone (no cold boot).
	before := f.createCount()
	m.reconcile(ctx())
	m.reconcile(ctx())
	if f.createCount() != before {
		t.Fatalf("reconcile cold-booted a slept app: creates %d→%d", before, f.createCount())
	}

	// Can't sleep an already-asleep app.
	if err := m.Sleep(ctx(), "web"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Sleep(asleep) err = %v, want ErrNotRunning", err)
	}

	// Wake restores it.
	if err := m.Wake(ctx(), "web"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if got, _ := m.GetByName("web"); got.Status.Phase != "running" {
		t.Fatalf("after wake: phase=%q, want running", got.Status.Phase)
	}

	// Sleep/Wake on an unknown app is ErrNotFound.
	if err := m.Sleep(ctx(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Sleep(unknown) err = %v, want ErrNotFound", err)
	}
}

// fakeActivity is an ActivitySource backed by a static map, for idle-monitor tests.
type fakeActivity map[string]activityRec

type activityRec struct {
	last     time.Time
	inflight int
}

func (f fakeActivity) Activity(name string) (time.Time, int, bool) {
	r, ok := f[name]
	return r.last, r.inflight, ok
}

func sleepableSpec(name string, idleSec, minScale int) api.AppSpec {
	s := nginxSpec(name, wire.RestartAlways)
	s.Sleep = &api.SleepPolicy{IdleTimeoutSec: idleSec, MinScale: minScale}
	return s
}

// TestIdleCheckSleepsIdleApp: a running, healthy scale-to-zero app with no
// in-flight requests, idle past its timeout, is auto-slept.
func TestIdleCheckSleepsIdleApp(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	mustCreate(t, m, sleepableSpec("web", 30, 0), true)
	m.reconcile(ctx()) // boots → running/healthy

	now := time.Unix(1_700_000_000, 0)
	m.SetActivitySource(fakeActivity{"web": {last: now.Add(-60 * time.Second), inflight: 0}})

	m.idleCheck(now)

	if got, _ := m.GetByName("web"); got.Status == nil || got.Status.Phase != "asleep" {
		t.Fatalf("idle app not slept: %v", got.Status)
	}
}

// TestIdleCheckLeavesNonIdle covers every reason NOT to auto-sleep.
func TestIdleCheckLeavesNonIdle(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name string
		spec func() api.AppSpec
		act  fakeActivity
	}{
		{"recent activity", func() api.AppSpec { return sleepableSpec("web", 30, 0) },
			fakeActivity{"web": {last: now.Add(-5 * time.Second)}}},
		{"in-flight request", func() api.AppSpec { return sleepableSpec("web", 30, 0) },
			fakeActivity{"web": {last: now.Add(-60 * time.Second), inflight: 1}}},
		{"no sleep policy", func() api.AppSpec { return nginxSpec("web", wire.RestartAlways) },
			fakeActivity{"web": {last: now.Add(-60 * time.Second)}}},
		{"min_scale keeps one warm", func() api.AppSpec { return sleepableSpec("web", 30, 1) },
			fakeActivity{"web": {last: now.Add(-60 * time.Second)}}},
		{"never seen through proxy", func() api.AppSpec { return sleepableSpec("web", 30, 0) },
			fakeActivity{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			m, _ := newMgr(t, f)
			mustCreate(t, m, tc.spec(), true)
			m.reconcile(ctx())
			m.SetActivitySource(tc.act)

			m.idleCheck(now)

			if got, _ := m.GetByName("web"); got.Status == nil || got.Status.Phase != "running" {
				t.Fatalf("app was slept but shouldn't be: %v", got.Status)
			}
		})
	}
}

// TestSleepWakeMutuallyExclusive is the M2-3 correctness centerpiece: a wake
// that arrives while a sleep is mid-flight must NOT interleave with it (which
// would observe a half-slept instance). It blocks on the per-app transition
// lock until the sleep completes, then resolves against a coherent asleep state.
func TestSleepWakeMutuallyExclusive(t *testing.T) {
	f := newFake()
	// Gate: block inside inst.Sleep until released, so the sleep holds the
	// transition lock while we fire a concurrent wake.
	entered := make(chan struct{})
	release := make(chan struct{})
	f.onSleep = func() { close(entered); <-release }
	m, _ := newMgr(t, f)

	mustCreate(t, m, nginxSpec("web", wire.RestartAlways), true)
	m.reconcile(ctx())

	sleepErr := make(chan error, 1)
	go func() { sleepErr <- m.Sleep(ctx(), "web") }()
	<-entered // sleep is now mid-flight, holding the transition lock

	wakeErr := make(chan error, 1)
	go func() { wakeErr <- m.Wake(ctx(), "web") }()

	// The wake must block on the transition lock while the sleep is mid-flight.
	select {
	case e := <-wakeErr:
		t.Fatalf("wake completed while sleep was mid-flight (err=%v) — not mutually exclusive", e)
	case <-time.After(100 * time.Millisecond):
		// good: wake is parked on the transition lock
	}

	// Let the sleep finish; the wake then runs against a coherent asleep state.
	close(release)
	if e := <-sleepErr; e != nil {
		t.Fatalf("Sleep: %v", e)
	}
	if e := <-wakeErr; e != nil {
		t.Fatalf("Wake after sleep completed: %v", e)
	}
	if got, _ := m.GetByName("web"); got.Status == nil || got.Status.Phase != "running" {
		t.Fatalf("after serialized sleep+wake: phase=%v, want running", got.Status)
	}
}

// TestReconcileLeavesAsleepAppAlone is the A2 guard: a slept (or mid-wake) app
// is a steady desired state. Even with its instance gone, the reconciler must
// NOT cold-boot a replacement — only an explicit Wake does that.
func TestReconcileLeavesAsleepAppAlone(t *testing.T) {
	for _, phase := range []string{"asleep", "waking"} {
		t.Run(phase, func(t *testing.T) {
			f := newFake()
			m, _ := newMgr(t, f)

			rec, _ := m.Create(nginxSpec("web", wire.RestartAlways), true)
			m.reconcile(ctx())
			if f.createCount() != 1 || f.liveCount() != 1 {
				t.Fatalf("after boot: creates=%d live=%d, want 1/1", f.createCount(), f.liveCount())
			}

			// Simulate a sleep: mark the phase and drop the live instance, as the
			// Sleep path (Group B/C) will. Without the guard, reconcile would see
			// a missing instance and cold-boot.
			m.obsMu.Lock()
			ob := m.obs[rec.ID]
			ob.phase = phase
			ob.instanceID = ""
			m.obsMu.Unlock()

			m.reconcile(ctx())
			m.reconcile(ctx())

			if f.createCount() != 1 {
				t.Fatalf("%s app got cold-booted: creates=%d, want 1", phase, f.createCount())
			}
			m.obsMu.Lock()
			got := m.obs[rec.ID].phase
			m.obsMu.Unlock()
			if got != phase {
				t.Fatalf("phase changed under reconcile: got %q, want %q", got, phase)
			}
		})
	}
}
