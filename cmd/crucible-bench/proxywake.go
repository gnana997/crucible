package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// proxyWake measures the PRODUCT wake number: a request hitting a slept,
// proxy-fronted app, timed from request start to the response being served — the
// wake restore happens inline while the proxy holds the request. This is the
// "wake in under a second" headline, end to end (unlike the sandbox-level
// wake_ms in the latency phase, which is the bare restore mechanism).
func (b *bench) proxyWake(ctx context.Context, proxyAddr, domain, image string) {
	fmt.Println("\n" + head.Render("⑤ proxy wake") + dim.Render(fmt.Sprintf("  (%d samples; %s via %s)", b.samples, image, proxyAddr)))

	const name = "bench-wake"
	_ = b.cl.DeleteApp(ctx, name) // clear any leftover from a prior run

	if _, err := b.cl.CreateApp(ctx, api.CreateAppRequest{AppSpec: api.AppSpec{
		Name:      name,
		Image:     &api.ImageRef{OCI: image},
		Pull:      "missing",
		Port:      80,
		MemoryMiB: b.memMiB,
		Restart:   wire.RestartPolicy{Policy: wire.RestartAlways},
	}}); err != nil {
		fatal("create app", err)
	}
	defer func() { _ = b.cl.DeleteApp(ctx, name) }()

	host := name + "." + domain
	hit := func() (time.Duration, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+proxyAddr+"/", nil)
		req.Host = host
		t0 := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		d := time.Since(t0)
		if resp.StatusCode != http.StatusOK {
			return d, fmt.Errorf("status %d", resp.StatusCode)
		}
		return d, nil
	}

	// Wait until the app boots and serves through the proxy.
	if !until(60*time.Second, func() bool { _, err := hit(); return err == nil }) {
		fatal("app never served via the proxy", fmt.Errorf("host %s", host))
	}

	wake := b.measure("proxy wake (req → served)", func() time.Duration {
		if _, err := b.cl.SleepApp(ctx, name); err != nil {
			fatal("sleep app", err)
		}
		if !until(30*time.Second, func() bool { r, _ := b.cl.GetApp(ctx, name); return r.Status != nil && r.Status.Phase == "asleep" }) {
			fatal("app never reached asleep", fmt.Errorf("app %s", name))
		}
		d, err := hit()
		if err != nil {
			fatal("proxy wake request", err)
		}
		return d
	})

	b.results["proxy_wake"] = map[string]any{"req_to_served_ms": wake, "image": image}
}

// until polls cond every 250ms until it returns true or the timeout elapses.
func until(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
