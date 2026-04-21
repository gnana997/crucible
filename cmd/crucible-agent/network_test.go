//go:build linux

package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubLinkRunner records ip-link-set invocations and returns a
// hook-supplied error per call. Installed at package scope for
// the duration of a test via withLinkRunner.
type stubLinkRunner struct {
	calls atomic.Int32
	hook  func(n int32, args []string) error

	mu  sync.Mutex
	log [][]string
}

func withLinkRunner(t *testing.T, hook func(n int32, args []string) error) *stubLinkRunner {
	t.Helper()
	s := &stubLinkRunner{hook: hook}
	orig := ipLinkRunner
	ipLinkRunner = func(_ context.Context, args ...string) error {
		n := s.calls.Add(1)
		s.mu.Lock()
		s.log = append(s.log, append([]string(nil), args...))
		s.mu.Unlock()
		return s.hook(n, args)
	}
	t.Cleanup(func() { ipLinkRunner = orig })
	return s
}

func (s *stubLinkRunner) snapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.log))
	for i, c := range s.log {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// withWait replaces the package-level waitForIP with a stub. The
// stub receives the context the handler passes, so tests can
// verify cancellation propagation.
func withWait(t *testing.T, hook func(ctx context.Context, iface string) error) {
	t.Helper()
	orig := waitForIP
	waitForIP = hook
	t.Cleanup(func() { waitForIP = orig })
}

func TestNetworkRefreshSuccessBouncesLinkAndWaitsForIP(t *testing.T) {
	runner := withLinkRunner(t, func(_ int32, _ []string) error { return nil })
	waitCalled := false
	withWait(t, func(_ context.Context, iface string) error {
		waitCalled = true
		if iface != guestIface {
			t.Errorf("waitForIP got iface %q, want %q", iface, guestIface)
		}
		return nil
	})

	req := httptest.NewRequest("POST", "/network/refresh", nil)
	w := httptest.NewRecorder()
	handleNetworkRefresh(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
	}
	if calls[0][0] != guestIface || calls[0][1] != "down" {
		t.Errorf("first call should be %q down, got %v", guestIface, calls[0])
	}
	if calls[1][0] != guestIface || calls[1][1] != "up" {
		t.Errorf("second call should be %q up, got %v", guestIface, calls[1])
	}
	if !waitCalled {
		t.Error("waitForIP should have been invoked after the link bounce")
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("body missing ok: %s", w.Body.String())
	}
}

func TestNetworkRefreshLinkDownFailureReturns500(t *testing.T) {
	runner := withLinkRunner(t, func(n int32, _ []string) error {
		if n == 1 {
			return errors.New("RTNETLINK answers: Operation not permitted")
		}
		return nil
	})
	withWait(t, func(context.Context, string) error {
		t.Error("waitForIP should not be reached if the down step fails")
		return nil
	})

	req := httptest.NewRequest("POST", "/network/refresh", nil)
	w := httptest.NewRecorder()
	handleNetworkRefresh(w, req)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "network refresh failed (down)") {
		t.Errorf("body should distinguish down-step failure: %s", w.Body.String())
	}
	if got := len(runner.snapshot()); got != 1 {
		t.Errorf("calls = %d, want 1 (up must not run after down fails)", got)
	}
}

func TestNetworkRefreshLinkUpFailureReturns500(t *testing.T) {
	runner := withLinkRunner(t, func(n int32, _ []string) error {
		if n == 1 {
			return nil // down OK
		}
		return errors.New("RTNETLINK answers: Network is unreachable")
	})
	waitWasCalled := false
	withWait(t, func(context.Context, string) error {
		waitWasCalled = true
		return nil
	})

	req := httptest.NewRequest("POST", "/network/refresh", nil)
	w := httptest.NewRecorder()
	handleNetworkRefresh(w, req)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "network refresh failed (up)") {
		t.Errorf("body should distinguish up-step failure: %s", w.Body.String())
	}
	if waitWasCalled {
		t.Error("waitForIP should not be reached if up fails")
	}
	if got := len(runner.snapshot()); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestNetworkRefreshWaitTimeoutReturns500(t *testing.T) {
	withLinkRunner(t, func(_ int32, _ []string) error { return nil })
	withWait(t, func(ctx context.Context, _ string) error {
		// Pretend we waited the full timeout and nothing showed up.
		return context.DeadlineExceeded
	})

	req := httptest.NewRequest("POST", "/network/refresh", nil)
	w := httptest.NewRecorder()
	handleNetworkRefresh(w, req)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "network refresh failed (wait") {
		t.Errorf("body should mention wait step: %s", w.Body.String())
	}
}

func TestNetworkRefreshContextPropagates(t *testing.T) {
	// Handler must pass the request context (with its timeout)
	// through to every seam: both link-runner calls and waitForIP.
	// Cancel the request context up front and assert the runner
	// sees a canceled context.
	var sawCtx context.Context
	withLinkRunner(t, func(_ int32, _ []string) error { return nil })
	orig := ipLinkRunner
	ipLinkRunner = func(ctx context.Context, args ...string) error {
		sawCtx = ctx
		return nil
	}
	t.Cleanup(func() { ipLinkRunner = orig })
	withWait(t, func(ctx context.Context, _ string) error { return ctx.Err() })

	req := httptest.NewRequest("POST", "/network/refresh", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	cancel()

	w := httptest.NewRecorder()
	handleNetworkRefresh(w, req)

	if sawCtx == nil {
		t.Fatal("runner was not invoked")
	}
	select {
	case <-sawCtx.Done():
		// good
	default:
		t.Error("runner context should be canceled after caller cancels")
	}
}

func TestHasUsableIPv4OnLoopback(t *testing.T) {
	// Loopback ("lo") has 127.0.0.1, which our predicate should
	// reject as loopback. Guards against a regression where we'd
	// return true as soon as eth0 has *any* IPv4.
	if hasUsableIPv4("lo") {
		t.Error("loopback should not count as usable IPv4")
	}
}

func TestHasUsableIPv4MissingInterface(t *testing.T) {
	// Non-existent interface must not panic; must return false.
	if hasUsableIPv4("this-iface-does-not-exist-hopefully") {
		t.Error("missing interface should not count as usable IPv4")
	}
}

func TestWaitForIfaceIPv4RespectsContext(t *testing.T) {
	// waitForIfaceIPv4 should return ctx.Err() promptly when the
	// context is canceled, rather than blocking forever on a
	// never-ready interface.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := waitForIfaceIPv4(ctx, "this-iface-does-not-exist-hopefully")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}
