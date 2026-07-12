package app

import (
	"testing"

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
