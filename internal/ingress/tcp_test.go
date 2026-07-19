package ingress

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startEchoBackend starts a TCP server that echoes each connection's bytes back,
// standing in for a guest service. Returns its host, port, and a closer.
func startEchoBackend(t *testing.T) (host string, port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }(c)
		}
	}()
	h, ps, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(ps)
	return h, p, func() { _ = ln.Close() }
}

// newWakeTestForwarder wires a WakingForwarder in front of an asleep app whose
// single instance points at an echo backend, with a blockingWaker that holds
// each wake until released (then flips the app to running). Mirrors
// newWakeTestProxy for the L4 path.
func newWakeTestForwarder(t *testing.T, act *ActivityTracker, reapIdle time.Duration) (*WakingForwarder, *blockingWaker, func()) {
	t.Helper()
	host, port, closeBackend := startEchoBackend(t)
	apps := &wakingApps{instance: "sbx_1", port: port, phase: "asleep"}
	inst := fakeInstances{ips: map[string]string{"sbx_1": host}}
	resolver := NewResolver(apps, inst, "", "", 0) // ttl 0: no cache; ResolveName by app name
	waker := &blockingWaker{apps: apps, release: make(chan struct{})}
	f, err := NewWakingForwarder(WakingForwarderConfig{
		HostAddr: "127.0.0.1:0", AppName: "pg", GuestPort: port,
		Resolver: resolver, Waker: waker, Activity: act, ReapIdle: reapIdle,
	})
	if err != nil {
		closeBackend()
		t.Fatalf("NewWakingForwarder: %v", err)
	}
	return f, waker, func() { f.Close(); closeBackend() }
}

// roundtrip writes payload to a fresh connection to f and returns the echo.
func roundtrip(t *testing.T, f *WakingForwarder, payload string) string {
	t.Helper()
	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	return string(buf)
}

func TestWakingForwarderWakesAsleepAppAndForwards(t *testing.T) {
	f, waker, cleanup := newWakeTestForwarder(t, nil, 0)
	defer cleanup()

	got := make(chan string, 1)
	go func() { got <- roundtrip(t, f, "ping") }()

	// The connection is held in the wake; it must not have echoed yet.
	select {
	case <-got:
		t.Fatal("connection forwarded before the wake was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(waker.release)

	select {
	case echo := <-got:
		if echo != "ping" {
			t.Fatalf("echo = %q, want %q", echo, "ping")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for forwarded echo after wake")
	}
	if n := atomic.LoadInt32(&waker.calls); n != 1 {
		t.Fatalf("wakes = %d, want 1", n)
	}
}

func TestWakingForwarderRunningForwardsWithoutWake(t *testing.T) {
	f, waker, cleanup := newWakeTestForwarder(t, nil, 0)
	defer cleanup()
	// App already running: no wake should fire.
	waker.apps.wakeToRunning()

	if echo := roundtrip(t, f, "hello"); echo != "hello" {
		t.Fatalf("echo = %q, want %q", echo, "hello")
	}
	if n := atomic.LoadInt32(&waker.calls); n != 0 {
		t.Fatalf("wakes = %d, want 0 (app was already running)", n)
	}
}

// TestWakingForwarderHerdCoalescesToOneWake — the money test: N concurrent
// connections to one slept app trigger exactly one wake, and all are forwarded.
func TestWakingForwarderHerdCoalescesToOneWake(t *testing.T) {
	f, waker, cleanup := newWakeTestForwarder(t, nil, 0)
	defer cleanup()

	const N = 30
	echoes := make([]string, N)
	var launched, done sync.WaitGroup
	launched.Add(N)
	done.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer done.Done()
			launched.Done()
			echoes[i] = roundtrip(t, f, "c"+strconv.Itoa(i))
		}(i)
	}
	launched.Wait()
	time.Sleep(60 * time.Millisecond) // let the whole herd pile onto the one flight
	close(waker.release)
	done.Wait()

	if n := atomic.LoadInt32(&waker.calls); n != 1 {
		t.Fatalf("herd of %d triggered %d wakes, want exactly 1", N, n)
	}
	for i, e := range echoes {
		if want := "c" + strconv.Itoa(i); e != want {
			t.Fatalf("conn %d echo = %q, want %q", i, e, want)
		}
	}
}

// TestWakingForwarderWakeFailClosesClient — a wake that never brings the app to
// running leaves it asleep, so the re-resolve returns ErrAsleep and the client is
// closed with no data (same as connecting to a stopped container).
func TestWakingForwarderWakeFailClosesClient(t *testing.T) {
	host, port, closeBackend := startEchoBackend(t)
	defer closeBackend()
	apps := &wakingApps{instance: "sbx_1", port: port, phase: "asleep"}
	inst := fakeInstances{ips: map[string]string{"sbx_1": host}}
	resolver := NewResolver(apps, inst, "", "", 0)
	// A waker that returns without flipping the app to running: it stays asleep.
	waker := wakerFunc(func(_ context.Context, _ string) error { return nil })
	f, err := NewWakingForwarder(WakingForwarderConfig{
		HostAddr: "127.0.0.1:0", AppName: "pg", GuestPort: port,
		Resolver: resolver, Waker: waker,
	})
	if err != nil {
		t.Fatalf("NewWakingForwarder: %v", err)
	}
	defer f.Close()

	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	// The forwarder never dials a backend (app never runs), so it closes our
	// connection: a read returns EOF with zero bytes.
	if n, err := c.Read(make([]byte, 8)); err != io.EOF || n != 0 {
		t.Fatalf("read after failed wake = (%d, %v), want (0, EOF)", n, err)
	}
}

func TestWakingForwarderRecordsActivity(t *testing.T) {
	act := NewActivityTracker()
	f, waker, cleanup := newWakeTestForwarder(t, act, 0)
	defer cleanup()
	waker.apps.wakeToRunning()

	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatalf("read: %v", err)
	}
	// The connection is open + forwarding: one in-flight.
	if _, inflight, ok := act.Activity("pg"); !ok || inflight != 1 {
		t.Fatalf("while open: inflight=%d ok=%v, want 1 true", inflight, ok)
	}

	_ = c.Close()
	// After close, in-flight drops to zero (last-close stamps the idle clock).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, inflight, ok := act.Activity("pg"); ok && inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			_, inflight, _ := act.Activity("pg")
			t.Fatalf("after close: inflight=%d, want 0", inflight)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWakingForwarderSeedsActivityOnStart(t *testing.T) {
	act := NewActivityTracker()
	// Before any forwarder exists, the app is unseen.
	if _, _, ok := act.Activity("pg"); ok {
		t.Fatal("app seen before any forwarder started")
	}
	f, _, cleanup := newWakeTestForwarder(t, act, 0)
	defer cleanup()
	_ = f
	// NewWakingForwarder calls Seen, so the idle monitor can now observe (and
	// eventually sleep) the app even with zero connections.
	last, inflight, ok := act.Activity("pg")
	if !ok || inflight != 0 || last.IsZero() {
		t.Fatalf("after start: ok=%v inflight=%d lastZero=%v, want true 0 false", ok, inflight, last.IsZero())
	}
}

// TestWakingForwarderReapsIdleConnection — with a reap timeout, a connection held
// byte-idle is closed by the forwarder (not the client), so in-flight drops to
// zero and the app can sleep even though a pooled client kept its socket open.
func TestWakingForwarderReapsIdleConnection(t *testing.T) {
	act := NewActivityTracker()
	f, waker, cleanup := newWakeTestForwarder(t, act, 250*time.Millisecond)
	defer cleanup()
	waker.apps.wakeToRunning()

	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	// One round-trip to establish the splice, then sit idle (send nothing).
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, inflight, _ := act.Activity("pg"); inflight != 1 {
		t.Fatalf("inflight=%d while connected, want 1", inflight)
	}

	// The forwarder reaps the idle connection after ~reapIdle; in-flight → 0 with
	// no action from the client.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, inflight, _ := act.Activity("pg"); inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			_, inflight, _ := act.Activity("pg")
			t.Fatalf("idle connection not reaped: inflight=%d, want 0", inflight)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The client observes the reap as a closed connection.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("client read after reap = %v, want EOF", err)
	}
}

// TestWakingForwarderKeepConnectionsNoReap — with reaping off (reapIdle 0), a
// byte-idle connection is NOT closed, so it stays counted (pub/sub: the app must
// not sleep out from under a live-but-quiet subscription).
func TestWakingForwarderKeepConnectionsNoReap(t *testing.T) {
	act := NewActivityTracker()
	f, waker, cleanup := newWakeTestForwarder(t, act, 0)
	defer cleanup()
	waker.apps.wakeToRunning()

	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Sit idle well past what a short reap would use; the connection stays counted.
	time.Sleep(500 * time.Millisecond)
	if _, inflight, _ := act.Activity("pg"); inflight != 1 {
		t.Fatalf("inflight=%d after idle with reaping off, want 1 (connection kept)", inflight)
	}
}

// TestWakingForwarderHoldsClientThroughNotReadyGuest is the regression for the
// cold-wake burst RST: a running app whose guest service isn't accepting yet
// (post-wake lazy-paging ramp / backlog overflow) must HOLD the client and retry
// the dial, then forward once the service is up — never reset the client.
func TestWakingForwarderHoldsClientThroughNotReadyGuest(t *testing.T) {
	// Grab a free port and leave it CLOSED so the first dials are refused.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, ps, _ := net.SplitHostPort(probe.Addr().String())
	port, _ := strconv.Atoi(ps)
	_ = probe.Close() // port now free → dials refuse until we bring the guest up

	apps := &wakingApps{instance: "sbx_1", port: port, phase: "running"} // running: no wake, straight to dial
	inst := fakeInstances{ips: map[string]string{"sbx_1": host}}
	resolver := NewResolver(apps, inst, "", "", 0)
	waker := &blockingWaker{apps: apps, release: make(chan struct{})}
	f, err := NewWakingForwarder(WakingForwarderConfig{
		HostAddr: "127.0.0.1:0", AppName: "pg", GuestPort: port,
		Resolver: resolver, Waker: waker,
	})
	if err != nil {
		t.Fatalf("NewWakingForwarder: %v", err)
	}
	defer f.Close()

	// Client connects while the guest is NOT accepting — must be held, not reset.
	got := make(chan string, 1)
	go func() { got <- roundtrip(t, f, "burst") }()
	select {
	case <-got:
		t.Fatal("forwarded before the guest was ready (should have been held)")
	case <-time.After(300 * time.Millisecond):
	}

	// Bring the guest up on the same port; the retry-dial must now succeed.
	ln, err := net.Listen("tcp", net.JoinHostPort(host, ps))
	if err != nil {
		t.Fatalf("bring guest up: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }(c)
		}
	}()

	select {
	case echo := <-got:
		if echo != "burst" {
			t.Fatalf("echo = %q, want %q", echo, "burst")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client not held through the not-ready guest — reset or never forwarded")
	}
}
