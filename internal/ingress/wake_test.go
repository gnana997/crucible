package ingress

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type wakerFunc func(ctx context.Context, name string) error

func (f wakerFunc) Wake(ctx context.Context, name string) error { return f(ctx, name) }

// TestWakeCoordinatorCoalescesHerd is the single-flight guarantee: N concurrent
// wake requests for one app trigger exactly ONE underlying Wake, and all get its
// result.
func TestWakeCoordinatorCoalescesHerd(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	w := wakerFunc(func(context.Context, string) error {
		atomic.AddInt32(&calls, 1)
		<-release // hold the flight open so the whole herd piles onto it
		return nil
	})
	c := newWakeCoordinator(w, time.Second)

	const N = 100
	var launched, done sync.WaitGroup
	launched.Add(N)
	done.Add(N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer done.Done()
			launched.Done()
			errs[i] = c.wake(context.Background(), "web")
		}(i)
	}
	launched.Wait()                   // all goroutines running...
	time.Sleep(30 * time.Millisecond) // ...and parked in the shared flight
	close(release)
	done.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("herd of %d triggered %d wakes, want exactly 1", N, got)
	}
	for i, e := range errs {
		if e != nil {
			t.Fatalf("waiter %d got %v, want nil", i, e)
		}
	}
}

// A completed flight is forgotten, so a later wake (app slept again) re-runs.
func TestWakeCoordinatorForgetsAfterCompletion(t *testing.T) {
	var calls int32
	w := wakerFunc(func(context.Context, string) error { atomic.AddInt32(&calls, 1); return nil })
	c := newWakeCoordinator(w, time.Second)

	for i := 0; i < 3; i++ {
		if err := c.wake(context.Background(), "web"); err != nil {
			t.Fatalf("wake %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("sequential wakes = %d calls, want 3", got)
	}
}

func TestWakeCoordinatorPropagatesError(t *testing.T) {
	boom := errors.New("wake failed")
	c := newWakeCoordinator(wakerFunc(func(context.Context, string) error { return boom }), time.Second)
	if err := c.wake(context.Background(), "web"); !errors.Is(err, boom) {
		t.Fatalf("wake err = %v, want %v", err, boom)
	}
}

// A caller that gives up (ctx cancelled) returns promptly and does NOT abort the
// shared wake — the other waiters still complete.
func TestWakeCoordinatorCallerCancelDoesNotAbortShared(t *testing.T) {
	proceed := make(chan struct{})
	w := wakerFunc(func(context.Context, string) error { <-proceed; return nil })
	c := newWakeCoordinator(w, time.Second)

	// Waiter A stays; waiter B gives up.
	aDone := make(chan error, 1)
	go func() { aDone <- c.wake(context.Background(), "web") }()

	bCtx, bCancel := context.WithCancel(context.Background())
	bDone := make(chan error, 1)
	go func() { bDone <- c.wake(bCtx, "web") }()

	time.Sleep(20 * time.Millisecond)
	bCancel() // B abandons its wait
	if err := <-bDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter err = %v, want context.Canceled", err)
	}

	// The shared wake is still in flight; release it and A must complete cleanly.
	close(proceed)
	if err := <-aDone; err != nil {
		t.Fatalf("surviving waiter err = %v, want nil (shared wake must not have aborted)", err)
	}
}
