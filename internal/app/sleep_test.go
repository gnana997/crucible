package app

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// TestAppSleepSurvivesRestart: a slept app survives a daemon restart. A
// new Manager over the SAME store re-adopts the app as asleep (not cold-boot),
// and a wake restores it from the durable snapshot into a fresh instance.
func TestAppSleepSurvivesRestart(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Boot + sleep under the first Manager.
	f1 := newFake()
	m1 := NewManager(store, f1, quietLog())
	if _, err := m1.Create(sleepableSpec("web", 30, 0), true); err != nil {
		t.Fatalf("create: %v", err)
	}
	m1.reconcile(ctx())
	if err := m1.Sleep(ctx(), "web"); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if rec, found, _ := store.GetByName("web"); !found || rec.AsleepSnapshotID == "" {
		t.Fatalf("AsleepSnapshotID not persisted: %+v", rec)
	}

	// "Restart": a fresh Manager over the same store, with empty observed state
	// and a fresh instantiator (the old live instances were reaped).
	f2 := newFake()
	m2 := NewManager(store, f2, quietLog())
	m2.reconcile(ctx())

	if got, _ := m2.GetByName("web"); got.Status == nil || got.Status.Phase != "asleep" {
		t.Fatalf("app not re-adopted as asleep after restart: %v", got.Status)
	}
	if f2.createCount() != 0 {
		t.Fatalf("cold-booted instead of re-adopting the slept app: creates=%d", f2.createCount())
	}

	// A wake restores from the durable snapshot into a fresh instance.
	if err := m2.Wake(ctx(), "web"); err != nil {
		t.Fatalf("wake after restart: %v", err)
	}
	got, _ := m2.GetByName("web")
	if got.Status == nil || got.Status.Phase != "running" || got.Status.InstanceID == "" {
		t.Fatalf("not running with a fresh instance after wake: %v", got.Status)
	}
	if rec, _, _ := store.GetByName("web"); rec.AsleepSnapshotID != "" {
		t.Fatalf("AsleepSnapshotID not cleared after wake: %q", rec.AsleepSnapshotID)
	}
}

func TestValidateSpecSleepPolicy(t *testing.T) {
	base := func(sp *api.SleepPolicy) api.AppSpec {
		s := nginxSpec("web", wire.RestartAlways)
		s.Sleep = sp
		s.Port = 80 // a wake trigger, so these cases test the numeric rules only
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
		{"multi-replica warm pool", &api.SleepPolicy{MinScale: 3}, true}, // v0.5.2: min_scale > 1 = N warm replicas
		{"negative min_scale", &api.SleepPolicy{MinScale: -1}, false},
		{"autoscale range", &api.SleepPolicy{MinScale: 1, MaxScale: 4}, true},
		{"scale-to-zero autoscale", &api.SleepPolicy{MinScale: 0, MaxScale: 5, TargetConcurrency: 10}, true},
		{"max below min", &api.SleepPolicy{MinScale: 3, MaxScale: 2}, false},
		{"negative max_scale", &api.SleepPolicy{MaxScale: -1}, false},
		{"negative target_concurrency", &api.SleepPolicy{TargetConcurrency: -1}, false},
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

// TestValidateSpecScaleToZeroWakeTrigger — a scale-to-zero app (min_scale 0 +
// idle_timeout) is valid only if it can be woken: an HTTP --port (proxy) or a
// published host port (-p, the L4 waking forwarder). With neither it is rejected,
// so it can't be silently stranded always-on. Holds for volume apps too.
func TestValidateSpecScaleToZeroWakeTrigger(t *testing.T) {
	s2z := &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 30}
	pub := []api.PortMapping{{HostPort: 5432, GuestPort: 5432}}
	vol := []api.VolumeMount{{Name: "pgdata", Path: "/var/lib/postgresql/data"}}
	spec := func(mut func(s *api.AppSpec)) api.AppSpec {
		s := nginxSpec("web", wire.RestartAlways)
		s.Sleep = s2z
		mut(&s)
		return s
	}
	cases := []struct {
		name string
		spec api.AppSpec
		ok   bool
	}{
		{"proxy port", spec(func(s *api.AppSpec) { s.Port = 80 }), true},
		{"published tcp port", spec(func(s *api.AppSpec) { s.Publish = pub }), true},
		{"no wake trigger", spec(func(*api.AppSpec) {}), false},
		{"volume + published port (serverless postgres)", spec(func(s *api.AppSpec) { s.Publish = pub; s.Volumes = vol }), true},
		{"volume, no wake trigger", spec(func(s *api.AppSpec) { s.Volumes = vol }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpec(tc.spec)
			if tc.ok && err != nil {
				t.Fatalf("validateSpec: unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("validateSpec: expected a wake-trigger error, got nil")
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

// TestVolumeAppSleepsViaStopStart: a volume-backed app sleeps by quiescing +
// destroying its instance (never a snapshot) and wakes by cold-creating a fresh
// one (V-M3 stop/start).
func TestVolumeAppSleepsViaStopStart(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)

	spec := sleepableSpec("db", 30, 0) // idle-sleep, min_scale 0
	spec.Port = 8080                   // proxy-fronted so idle-sleep is permitted
	spec.Volumes = []api.VolumeMount{{Name: "data", Path: "/data"}}
	mustCreate(t, m, spec, true)
	m.reconcile(ctx())
	if got, _ := m.GetByName("db"); got.Status == nil || got.Status.Phase != "running" {
		t.Fatalf("phase after boot = %v, want running", got.Status)
	}

	// Sleep a volume app: quiesce (sync) + destroy, NEVER a snapshot.
	qBefore, dBefore := len(f.quiesces), len(f.destroys)
	if err := m.Sleep(ctx(), "db"); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if len(f.quiesces) <= qBefore {
		t.Error("volume-app sleep did not quiesce (sync) the instance")
	}
	if len(f.destroys) <= dBefore {
		t.Error("volume-app sleep did not destroy the instance (stop/start)")
	}
	if got, _ := m.GetByName("db"); got.Status.Phase != "asleep" {
		t.Fatalf("phase after sleep = %q, want asleep", got.Status.Phase)
	}

	// Wake a volume app: cold-create a fresh instance (no snapshot to restore).
	cBefore := f.createCount()
	if err := m.Wake(ctx(), "db"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if f.createCount() != cBefore+1 {
		t.Errorf("volume-app wake did not cold-create: creates %d→%d", cBefore, f.createCount())
	}
	if got, _ := m.GetByName("db"); got.Status.Phase != "running" {
		t.Fatalf("phase after wake = %q, want running", got.Status.Phase)
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
	s.Port = 80 // a wake trigger (proxy); required for a valid scale-to-zero app
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

// TestSleepWakeMutuallyExclusive is the correctness centerpiece: a wake
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

// TestReconcileLeavesAsleepAppAlone is the reconciler guard: a slept (or mid-wake) app
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
			// Sleep path will. Without the guard, reconcile would see
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
