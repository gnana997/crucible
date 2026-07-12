package ingress

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

// wakingApps is a mutable AppLookup whose app is "asleep" until wakeToRunning
// flips it, letting a test drive the proxy's wake-and-forward path. Each
// GetByName builds a FRESH *AppStatus under the lock (mirroring the real
// Manager.toResponse), so the resolver's later read of it never races the flip.
type wakingApps struct {
	mu       sync.Mutex
	instance string
	port     int
	phase    string
}

func (a *wakingApps) GetByName(name string) (api.AppResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := api.AppResponse{ID: "app_" + name}
	r.Name = name
	r.Port = a.port
	r.Status = &api.AppStatus{InstanceID: a.instance, Phase: a.phase}
	if a.instance != "" {
		r.Status.Instances = []api.InstanceStatus{{InstanceID: a.instance}}
	}
	return r, nil
}
func (a *wakingApps) wakeToRunning() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.phase = "running"
}

// blockingWaker counts wakes and holds each open until released, so a herd piles
// onto one in-flight wake. On completion it flips the app to running.
type blockingWaker struct {
	apps    *wakingApps
	calls   int32
	release chan struct{}
}

func (w *blockingWaker) Wake(context.Context, string) error {
	atomic.AddInt32(&w.calls, 1)
	<-w.release
	w.apps.wakeToRunning()
	return nil
}

func newWakeTestProxy(t *testing.T, onWake func(time.Duration)) (*Proxy, *wakingApps, *blockingWaker, func()) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served"))
	}))
	host, portStr, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	apps := &wakingApps{instance: "sbx_1", port: port, phase: "asleep"}
	inst := fakeInstances{ips: map[string]string{"sbx_1": host}}
	resolver := NewResolver(apps, inst, "apps.local", "", 0) // ttl 0: no cache
	waker := &blockingWaker{apps: apps, release: make(chan struct{})}
	p := New(Config{Resolver: resolver, Waker: waker, OnWake: onWake})
	return p, apps, waker, backend.Close
}

func TestProxyWakesAsleepAppAndForwards(t *testing.T) {
	p, _, waker, closeBackend := newWakeTestProxy(t, nil)
	defer closeBackend()

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, httptest.NewRequest("GET", "http://web.apps.local/", nil))
		close(done)
	}()

	// The request is held in the wake; it must not have completed yet.
	select {
	case <-done:
		t.Fatal("request completed before the wake was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(waker.release)
	<-done

	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("got %d %q, want 200 'served' (app should have woken + forwarded)", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&waker.calls); got != 1 {
		t.Fatalf("wakes = %d, want 1", got)
	}
}

// TestProxyReportsWakeLatency: a successful wake-on-request reports its latency
// via OnWake.
func TestProxyReportsWakeLatency(t *testing.T) {
	var observed int32
	p, _, waker, closeBackend := newWakeTestProxy(t, func(time.Duration) { atomic.AddInt32(&observed, 1) })
	defer closeBackend()
	close(waker.release) // wake without blocking

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "http://web.apps.local/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt32(&observed); got != 1 {
		t.Fatalf("OnWake called %d times, want 1", got)
	}
}

// TestProxyHerdCoalescesToOneWake — the money test: N concurrent requests to one
// slept app trigger exactly one wake, and all are served.
func TestProxyHerdCoalescesToOneWake(t *testing.T) {
	p, _, waker, closeBackend := newWakeTestProxy(t, nil)
	defer closeBackend()

	const N = 50
	recs := make([]*httptest.ResponseRecorder, N)
	var launched, done sync.WaitGroup
	launched.Add(N)
	done.Add(N)
	for i := 0; i < N; i++ {
		recs[i] = httptest.NewRecorder()
		go func(i int) {
			defer done.Done()
			launched.Done()
			p.ServeHTTP(recs[i], httptest.NewRequest("GET", "http://web.apps.local/", nil))
		}(i)
	}
	launched.Wait()
	time.Sleep(40 * time.Millisecond) // let the whole herd pile onto the flight
	close(waker.release)
	done.Wait()

	if got := atomic.LoadInt32(&waker.calls); got != 1 {
		t.Fatalf("herd of %d triggered %d wakes, want exactly 1", N, got)
	}
	for i, rec := range recs {
		if rec.Code != http.StatusOK || rec.Body.String() != "served" {
			t.Fatalf("request %d got %d %q, want 200 'served'", i, rec.Code, rec.Body.String())
		}
	}
}
