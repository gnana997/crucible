package ingress

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"
)

// defaultWakeTimeout bounds a proxy-triggered wake. app.Manager.Wake also bounds
// itself to guest-memory size; this is the proxy-level ceiling behind the clean
// 503-on-timeout story.
const defaultWakeTimeout = 30 * time.Second

// Waker triggers a wake of a slept app by name, blocking until it is running (or
// the wake fails / times out). Satisfied by *app.Manager (its Wake(ctx, name)).
type Waker interface {
	Wake(ctx context.Context, appName string) error
}

// wakeCoordinator collapses a herd of concurrent wake requests for one app into
// a single underlying Wake (singleflight): 100 requests hitting a slept app
// trigger exactly one restore, and all share its result. singleflight forgets
// the key when the call returns, so a later sleep/wake cycle starts a fresh
// flight.
type wakeCoordinator struct {
	waker   Waker
	timeout time.Duration
	sf      singleflight.Group
}

func newWakeCoordinator(w Waker, timeout time.Duration) *wakeCoordinator {
	if timeout <= 0 {
		timeout = defaultWakeTimeout
	}
	return &wakeCoordinator{waker: w, timeout: timeout}
}

// wake ensures appName is woken, coalescing concurrent callers. It returns when
// the shared wake completes or the caller's ctx is cancelled.
//
// The underlying Wake runs on a fresh bounded context — deliberately NOT the
// caller's — because singleflight shares one execution across all waiters: if it
// ran on the first caller's request context, that caller disconnecting would
// abort the wake for everyone else still waiting. Each caller instead selects on
// its own ctx to decide whether to keep waiting.
func (c *wakeCoordinator) wake(ctx context.Context, appName string) error {
	ch := c.sf.DoChan(appName, func() (any, error) {
		wctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		return nil, c.waker.Wake(wctx, appName)
	})
	select {
	case res := <-ch:
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}
