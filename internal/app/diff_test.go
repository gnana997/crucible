package app

import (
	"errors"
	"testing"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

func TestClassifyUpdate(t *testing.T) {
	base := func() api.AppSpec { return nginxSpec("web", wire.RestartAlways) }
	wakeSpec := func() api.AppSpec {
		s := base()
		s.Publish = []api.PortMapping{{HostPort: 18080, GuestPort: 80}}
		s.Sleep = &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 60}
		return s
	}
	cases := []struct {
		name string
		old  func() api.AppSpec
		mut  func(*api.AppSpec)
		want updateClass
	}{
		{"identical", base, func(s *api.AppSpec) {}, updateNoop},
		{"image", base, func(s *api.AppSpec) { s.Image = &api.ImageRef{OCI: "redis:7"} }, updateRedeploy},
		{"memory", base, func(s *api.AppSpec) { s.MemoryMiB = 512 }, updateRedeploy},
		{"env", base, func(s *api.AppSpec) { s.Env = map[string]string{"A": "1"} }, updateRedeploy},
		{"secret_env_from", base, func(s *api.AppSpec) { s.SecretEnvFrom = []string{"db"} }, updateRedeploy},
		{"volumes", base, func(s *api.AppSpec) { s.Volumes = []api.VolumeMount{{Name: "data", Path: "/data"}} }, updateRedeploy},

		{"sleep_policy", base, func(s *api.AppSpec) { s.Sleep = &api.SleepPolicy{MinScale: 1} }, updateInPlace},
		{"can_call", base, func(s *api.AppSpec) { s.CanCall = []string{"peer"} }, updateInPlace},
		{"restart", base, func(s *api.AppSpec) { s.Restart = wire.RestartPolicy{Policy: wire.RestartOnFailure} }, updateInPlace},
		{"health", base, func(s *api.AppSpec) { s.Health = &api.HealthCheck{Type: "tcp", Port: 80} }, updateInPlace},
		{"metrics_port", base, func(s *api.AppSpec) { s.MetricsPort = 9187 }, updateInPlace},

		// The wake-on-TCP predicate decides at boot whether the instance binds
		// its own published port; flipping it needs the rebuild.
		{"wake_mode_flip_disable_sleep", wakeSpec, func(s *api.AppSpec) { s.Sleep = nil }, updateRedeploy},
		{"wake_mode_flip_min_scale", wakeSpec, func(s *api.AppSpec) { s.Sleep.MinScale = 1 }, updateRedeploy},
		{"wake_same_mode_idle_timeout", wakeSpec, func(s *api.AppSpec) { s.Sleep.IdleTimeoutSec = 1800 }, updateInPlace},
		{"wake_same_mode_publish_change", wakeSpec, func(s *api.AppSpec) { s.Publish[0].HostPort = 19090 }, updateInPlace},

		// Ordinary (non-wake) publish is bound per instance at sandbox create.
		{"publish_change_non_wake", func() api.AppSpec {
			s := base()
			s.Publish = []api.PortMapping{{HostPort: 18080, GuestPort: 80}}
			return s
		}, func(s *api.AppSpec) { s.Publish[0].HostPort = 19090 }, updateRedeploy},

		// NIC-need flips: the instance was built with no NIC to route to.
		{"first_internal_port_nic_flip", base, func(s *api.AppSpec) {
			s.InternalPorts = []api.InternalPort{{Port: 5432}}
		}, updateRedeploy},
		{"internal_ports_change_same_nic", func() api.AppSpec {
			s := base()
			s.InternalPorts = []api.InternalPort{{Port: 5432}}
			return s
		}, func(s *api.AppSpec) {
			s.InternalPorts = append(s.InternalPorts, api.InternalPort{Port: 6432})
		}, updateInPlace},
		{"first_proxy_port_nic_flip", base, func(s *api.AppSpec) { s.Port = 8080 }, updateRedeploy},

		// Egress policy reprograms in place on an instance that has a NIC;
		// gaining or losing the NIC itself is a rebuild.
		{"network_change_same_nic", func() api.AppSpec {
			s := base()
			s.Network = &api.NetworkRequest{Enabled: true, Allowlist: []string{"pypi.org"}}
			return s
		}, func(s *api.AppSpec) { s.Network.Allowlist = []string{"pypi.org", "files.pythonhosted.org"} }, updateInPlace},
		{"network_added_no_nic", base, func(s *api.AppSpec) {
			s.Network = &api.NetworkRequest{Enabled: true, FullEgress: true}
		}, updateRedeploy},
		{"network_removed_keeps_nic", func() api.AppSpec {
			s := base()
			s.Port = 8080
			s.Network = &api.NetworkRequest{Enabled: true, Allowlist: []string{"pypi.org"}}
			return s
		}, func(s *api.AppSpec) { s.Network = nil }, updateInPlace},
		// A published app already has a (deny-all) NIC — adding egress to it is
		// a reprogram, not a rebuild.
		{"network_added_with_publish_nic", func() api.AppSpec {
			s := base()
			s.Publish = []api.PortMapping{{HostPort: 18080, GuestPort: 80}}
			return s
		}, func(s *api.AppSpec) {
			s.Network = &api.NetworkRequest{Enabled: true, FullEgress: true}
		}, updateInPlace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := tc.old()
			next := tc.old()
			tc.mut(&next)
			if got := classifyUpdate(old, next); got != tc.want {
				t.Errorf("classifyUpdate = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestUpdateSleepOnlyChangeAppliesInPlace(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Port = 8080 // the scale-to-zero wake trigger
	spec.Sleep = &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 300}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx())
	old := instanceOf(t, m, rec.ID)

	upd := nginxSpec("web", wire.RestartAlways)
	upd.Port = 8080
	upd.Sleep = &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 1800}
	got, redeployed, err := m.Update("web", upd)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if redeployed {
		t.Error("sleep-only change reported redeployed")
	}
	if got.Generation != rec.Generation {
		t.Errorf("generation = %d, want %d (unchanged — no rebuild)", got.Generation, rec.Generation)
	}
	if got.SpecRevision != rec.SpecRevision+1 {
		t.Errorf("spec_revision = %d, want %d", got.SpecRevision, rec.SpecRevision+1)
	}
	m.reconcile(ctx())
	m.reconcile(ctx())
	if cur := instanceOf(t, m, rec.ID); cur != old {
		t.Errorf("instance churned: %s → %s", old, cur)
	}
	if f.createCount() != 1 {
		t.Errorf("creates = %d, want 1 (no reboot)", f.createCount())
	}
	resp, _ := m.Get(rec.ID)
	if resp.Sleep == nil || resp.Sleep.IdleTimeoutSec != 1800 {
		t.Errorf("new sleep policy not persisted: %+v", resp.Sleep)
	}
}

func TestUpdateIdenticalSpecIsNoop(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	rec := mustCreate(t, m, nginxSpec("web", wire.RestartAlways), true)
	m.reconcile(ctx())

	got, redeployed, err := m.Update("web", nginxSpec("web", wire.RestartAlways))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if redeployed || got.Generation != rec.Generation || got.SpecRevision != rec.SpecRevision {
		t.Errorf("identical spec: redeployed=%v gen=%d rev=%d, want no-op (%d/%d)",
			redeployed, got.Generation, got.SpecRevision, rec.Generation, rec.SpecRevision)
	}
	m.reconcile(ctx())
	if f.createCount() != 1 {
		t.Errorf("creates = %d, want 1 (no-op must not reboot)", f.createCount())
	}
}

func TestUpdateCanCallAppliesInPlace(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	rec := mustCreate(t, m, nginxSpec("web", wire.RestartAlways), true)
	m.reconcile(ctx())
	old := instanceOf(t, m, rec.ID)

	upd := nginxSpec("web", wire.RestartAlways)
	upd.CanCall = []string{"peer"}
	if _, redeployed, err := m.Update("web", upd); err != nil || redeployed {
		t.Fatalf("Update: err=%v redeployed=%v, want in-place", err, redeployed)
	}
	if !m.CanCall("web", "peer") {
		t.Error("added peer not authorized after in-place update")
	}
	upd.CanCall = nil
	if _, redeployed, err := m.Update("web", upd); err != nil || redeployed {
		t.Fatalf("Update (revoke): err=%v redeployed=%v, want in-place", err, redeployed)
	}
	if m.CanCall("web", "peer") {
		t.Error("removed peer still authorized")
	}
	m.reconcile(ctx())
	if cur := instanceOf(t, m, rec.ID); cur != old {
		t.Errorf("instance churned: %s → %s", old, cur)
	}
}

func TestUpdateWakeModeFlipRedeploys(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Publish = []api.PortMapping{{HostPort: 18080, GuestPort: 80}}
	spec.Sleep = &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 60}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx())
	old := instanceOf(t, m, rec.ID)

	// Disabling sleep flips who binds the host port (forwarder → instance):
	// the running instance was built with its own bind suppressed.
	upd := nginxSpec("web", wire.RestartAlways)
	upd.Publish = []api.PortMapping{{HostPort: 18080, GuestPort: 80}}
	got, redeployed, err := m.Update("web", upd)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !redeployed || got.Generation != rec.Generation+1 {
		t.Errorf("wake-mode flip: redeployed=%v gen=%d, want redeploy at gen %d",
			redeployed, got.Generation, rec.Generation+1)
	}
	m.reconcile(ctx())
	if cur := instanceOf(t, m, rec.ID); cur == old || cur == "" {
		t.Errorf("instance not rebuilt: old=%s cur=%s", old, cur)
	}
}

func TestUpdateHealthChangeRearmsProbe(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Health = &api.HealthCheck{Type: "tcp", Port: 80, IntervalSec: 30}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx()) // boot schedules the first probe on the old interval

	upd := nginxSpec("web", wire.RestartAlways)
	upd.Health = &api.HealthCheck{Type: "tcp", Port: 80, IntervalSec: 5}
	if _, redeployed, err := m.Update("web", upd); err != nil || redeployed {
		t.Fatalf("Update: err=%v redeployed=%v, want in-place", err, redeployed)
	}
	m.obsMu.Lock()
	next := m.obs[rec.ID].nextProbe
	m.obsMu.Unlock()
	if !next.IsZero() {
		t.Errorf("nextProbe not re-armed after health change: %v", next)
	}
	if f.createCount() != 1 {
		t.Errorf("creates = %d, want 1", f.createCount())
	}
}

func TestUpdateHostSideWhileAsleepDoesNotWake(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Port = 8080
	spec.Sleep = &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 300}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx())

	// Simulate a slept app, as the Sleep path leaves it.
	m.obsMu.Lock()
	m.obs[rec.ID].phase = "asleep"
	m.obs[rec.ID].instanceID = ""
	m.obsMu.Unlock()

	upd := nginxSpec("web", wire.RestartAlways)
	upd.Port = 8080
	upd.Sleep = &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 1800}
	if _, redeployed, err := m.Update("web", upd); err != nil || redeployed {
		t.Fatalf("Update: err=%v redeployed=%v, want in-place", err, redeployed)
	}
	m.reconcile(ctx())
	m.reconcile(ctx())
	if f.createCount() != 1 {
		t.Errorf("asleep app booted by a host-side update: creates=%d, want 1", f.createCount())
	}
	m.obsMu.Lock()
	phase := m.obs[rec.ID].phase
	m.obsMu.Unlock()
	if phase != "asleep" {
		t.Errorf("phase = %q, want asleep (update must not wake)", phase)
	}
}

func TestUpdateNetworkChangeReprogramsInPlace(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Network = &api.NetworkRequest{Enabled: true, Allowlist: []string{"pypi.org"}}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx())
	old := instanceOf(t, m, rec.ID)

	upd := nginxSpec("web", wire.RestartAlways)
	upd.Network = &api.NetworkRequest{Enabled: true, Allowlist: []string{"pypi.org", "files.pythonhosted.org"}}
	got, redeployed, err := m.Update("web", upd)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if redeployed || got.Generation != rec.Generation {
		t.Errorf("network change: redeployed=%v gen=%d, want in-place at gen %d",
			redeployed, got.Generation, rec.Generation)
	}
	f.mu.Lock()
	reprograms := append([]string(nil), f.reprograms...)
	f.mu.Unlock()
	if len(reprograms) != 1 || reprograms[0] != old {
		t.Errorf("reprograms = %v, want exactly [%s]", reprograms, old)
	}
	if f.createCount() != 1 {
		t.Errorf("creates = %d, want 1 (no reboot)", f.createCount())
	}
	resp, _ := m.Get(rec.ID)
	if resp.Network == nil || len(resp.Network.Allowlist) != 2 {
		t.Errorf("new network policy not persisted: %+v", resp.Network)
	}
}

func TestUpdateNetworkReprogramFailureRejectsUpdate(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Network = &api.NetworkRequest{Enabled: true, Allowlist: []string{"pypi.org"}}
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx())

	f.mu.Lock()
	f.reprogramErr = errors.New("nft exploded")
	f.mu.Unlock()

	upd := nginxSpec("web", wire.RestartAlways)
	upd.Network = &api.NetworkRequest{Enabled: true, FullEgress: true}
	if _, _, err := m.Update("web", upd); err == nil {
		t.Fatal("Update succeeded despite reprogram failure")
	}
	// The stored spec (and generation) are untouched — no spec/policy mismatch.
	resp, _ := m.Get(rec.ID)
	if resp.Network == nil || resp.Network.FullEgress || len(resp.Network.Allowlist) != 1 {
		t.Errorf("rejected update mutated the stored spec: %+v", resp.Network)
	}
	if resp.Generation != rec.Generation || resp.SpecRevision != rec.SpecRevision {
		t.Errorf("gen/rev moved on a rejected update: %d/%d", resp.Generation, resp.SpecRevision)
	}
}

func TestUpdateMixedChangeRedeploys(t *testing.T) {
	f := newFake()
	m, _ := newMgr(t, f)
	spec := nginxSpec("web", wire.RestartAlways)
	spec.Port = 8080
	rec := mustCreate(t, m, spec, true)
	m.reconcile(ctx())

	// Host-side (sleep) and instance-defining (memory) together → the reboot
	// applies both.
	upd := nginxSpec("web", wire.RestartAlways)
	upd.Port = 8080
	upd.Sleep = &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 300}
	upd.MemoryMiB = 512
	got, redeployed, err := m.Update("web", upd)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !redeployed || got.Generation != rec.Generation+1 {
		t.Errorf("mixed change: redeployed=%v gen=%d, want redeploy at gen %d",
			redeployed, got.Generation, rec.Generation+1)
	}
}
