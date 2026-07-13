package ingress

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

// freePort grabs an ephemeral port and releases it, so a forwarder can bind it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

// newTestSet builds a WakeForwarderSet whose one app ("pg") is running and points
// at the given echo backend, with a no-op waker (the app is already up).
func newTestSet(backendHost string, backendPort int) *WakeForwarderSet {
	apps := &wakingApps{instance: "sbx_1", port: backendPort, phase: "running"}
	inst := fakeInstances{ips: map[string]string{"sbx_1": backendHost}}
	resolver := NewResolver(apps, inst, "", "", 0)
	waker := wakerFunc(func(context.Context, string) error { return nil })
	return NewWakeForwarderSet(resolver, waker, NewActivityTracker(), nil)
}

func scaleToZeroSpec(name, hostIP string, hostPort, guestPort int) api.AppSpec {
	return api.AppSpec{
		Name:    name,
		Sleep:   &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 30},
		Publish: []api.PortMapping{{HostIP: hostIP, HostPort: hostPort, GuestPort: guestPort}},
	}
}

func TestReconcilePortsBindsForwardsAndCloses(t *testing.T) {
	host, port, closeBackend := startEchoBackend(t)
	defer closeBackend()
	s := newTestSet(host, port)
	defer s.Close()

	hp := freePort(t)
	spec := scaleToZeroSpec("pg", "127.0.0.1", hp, port)

	// Reconcile → one forwarder bound on the host port, fronting the app.
	s.ReconcilePorts([]api.AppSpec{spec})
	if n := len(s.apps); n != 1 {
		t.Fatalf("bound apps = %d, want 1", n)
	}

	// It forwards to the (running) backend.
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	if err != nil {
		t.Fatalf("dial host port: %v", err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want ping", buf)
	}
	_ = c.Close()

	// Removed from desired state → forwarder closed, host port freed.
	s.ReconcilePorts(nil)
	if n := len(s.apps); n != 0 {
		t.Fatalf("bound apps after removal = %d, want 0", n)
	}
	// The port is bindable again (the forwarder released it).
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp)))
	if err != nil {
		t.Fatalf("host port not released after close: %v", err)
	}
	_ = ln.Close()
}

func TestReconcilePortsSkipsNonQualifying(t *testing.T) {
	host, port, closeBackend := startEchoBackend(t)
	defer closeBackend()
	s := newTestSet(host, port)
	defer s.Close()

	always := scaleToZeroSpec("always", "127.0.0.1", freePort(t), port)
	always.Sleep.MinScale = 1 // always-on → per-instance publish, no waking forwarder

	noPublish := api.AppSpec{Name: "np", Sleep: &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 30}}

	noSleep := api.AppSpec{Name: "ns", Publish: []api.PortMapping{{HostIP: "127.0.0.1", HostPort: freePort(t), GuestPort: port}}}

	s.ReconcilePorts([]api.AppSpec{always, noPublish, noSleep})
	if n := len(s.apps); n != 0 {
		t.Fatalf("bound apps = %d, want 0 (none qualify)", n)
	}
}

func TestReconcilePortsRebindsOnMappingChange(t *testing.T) {
	host, port, closeBackend := startEchoBackend(t)
	defer closeBackend()
	s := newTestSet(host, port)
	defer s.Close()

	hp1 := freePort(t)
	s.ReconcilePorts([]api.AppSpec{scaleToZeroSpec("pg", "127.0.0.1", hp1, port)})
	first := s.apps["pg"]
	if first == nil {
		t.Fatal("app not bound on first reconcile")
	}

	// Same signature → no rebind (identity preserved).
	s.ReconcilePorts([]api.AppSpec{scaleToZeroSpec("pg", "127.0.0.1", hp1, port)})
	if s.apps["pg"] != first {
		t.Fatal("unchanged mapping should not rebind")
	}

	// Changed host port → rebind (new forwarder object) and the old port frees.
	hp2 := freePort(t)
	s.ReconcilePorts([]api.AppSpec{scaleToZeroSpec("pg", "127.0.0.1", hp2, port)})
	if s.apps["pg"] == first {
		t.Fatal("changed mapping should rebind")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hp1)))
	if err != nil {
		t.Fatalf("old host port not released on rebind: %v", err)
	}
	_ = ln.Close()
}

func TestReapPolicy(t *testing.T) {
	s2z := func(mut func(*api.SleepPolicy)) api.AppSpec {
		sp := &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 30}
		mut(sp)
		return api.AppSpec{Sleep: sp}
	}
	cases := []struct {
		name     string
		spec     api.AppSpec
		wantReap time.Duration
		wantKeep bool
	}{
		{"default reap = idle_timeout", s2z(func(*api.SleepPolicy) {}), 30 * time.Second, false},
		{"explicit conn idle", s2z(func(sp *api.SleepPolicy) { sp.ConnIdleTimeoutSec = 5 }), 5 * time.Second, false},
		{"keep-connections → no reap + keepalive", s2z(func(sp *api.SleepPolicy) { sp.KeepConnections = true }), 0, true},
		{"keep-connections wins over conn idle", s2z(func(sp *api.SleepPolicy) { sp.ConnIdleTimeoutSec = 5; sp.KeepConnections = true }), 0, true},
	}
	for _, tc := range cases {
		reap, keep := reapPolicy(tc.spec)
		if reap != tc.wantReap || keep != tc.wantKeep {
			t.Errorf("%s: reapPolicy = (%v, %v), want (%v, %v)", tc.name, reap, keep, tc.wantReap, tc.wantKeep)
		}
	}
}

// TestReconcilePortsRebindsOnKeepConnectionsChange — toggling --keep-connections
// via app update must rebind the forwarder (its reap behavior changed), even
// though the host→guest mapping is unchanged.
func TestReconcilePortsRebindsOnKeepConnectionsChange(t *testing.T) {
	host, port, closeBackend := startEchoBackend(t)
	defer closeBackend()
	s := newTestSet(host, port)
	defer s.Close()

	hp := freePort(t)
	spec := scaleToZeroSpec("pg", "127.0.0.1", hp, port)
	s.ReconcilePorts([]api.AppSpec{spec})
	first := s.apps["pg"]
	if first == nil {
		t.Fatal("app not bound")
	}

	spec.Sleep.KeepConnections = true
	s.ReconcilePorts([]api.AppSpec{spec})
	if s.apps["pg"] == first {
		t.Fatal("toggling keep-connections should rebind the forwarder")
	}
}

func TestWakesOnTCP(t *testing.T) {
	pub := []api.PortMapping{{HostPort: 5432, GuestPort: 5432}}
	cases := []struct {
		name string
		spec api.AppSpec
		want bool
	}{
		{"scale-to-zero published", api.AppSpec{Sleep: &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 30}, Publish: pub}, true},
		{"no sleep policy", api.AppSpec{Publish: pub}, false},
		{"always-on (min_scale 1)", api.AppSpec{Sleep: &api.SleepPolicy{MinScale: 1, IdleTimeoutSec: 30}, Publish: pub}, false},
		{"no idle timeout", api.AppSpec{Sleep: &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 0}, Publish: pub}, false},
		{"scale-to-zero but no publish", api.AppSpec{Sleep: &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 30}}, false},
	}
	for _, tc := range cases {
		if got := WakesOnTCP(tc.spec); got != tc.want {
			t.Errorf("%s: WakesOnTCP = %v, want %v", tc.name, got, tc.want)
		}
	}
}
