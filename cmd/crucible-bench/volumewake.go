package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// volumeWake measures the v0.6.2 mechanism number: how long a VOLUME-backed
// app takes to snapshot-wake in place. Where the proxywake phase measures a
// stateless app, this attaches a persistent volume, so it exercises the full snapshot-wake
// path — restore the snapshot, re-stage + re-attach the volume drive, resume —
// and confirms a volume adds no wake overhead versus a stateless app.
//
// It reports two things per wake: the wall-clock time from the wake trigger to
// the instance running again (via measure()), and the daemon's own precise
// restore latency (last_wake_latency_ms on the app status). Needs a daemon with
// a --volume-dir and an image whose embedded agent can mount a volume (any OCI
// image; redis:alpine by default — small and long-running).
func (b *bench) volumeWake(ctx context.Context, image string) {
	fmt.Println("\n" + head.Render("⑥ volume wake") + dim.Render(fmt.Sprintf("  (%d samples; %s on a volume, snapshot-wake in place)", b.samples, image)))

	const name = "bench-volwake"
	const vol = "bench-volwake"
	_ = b.cl.DeleteApp(ctx, name) // clear any leftover from a prior run

	if _, err := b.cl.CreateApp(ctx, api.CreateAppRequest{AppSpec: api.AppSpec{
		Name:      name,
		Image:     &api.ImageRef{OCI: image},
		Pull:      "missing",
		MemoryMiB: b.memMiB,
		Volumes:   []api.VolumeMount{{Name: vol, Path: "/data"}},
		Restart:   wire.RestartPolicy{Policy: wire.RestartAlways},
	}}); err != nil {
		fatal("create volume app", err)
	}
	defer func() {
		_ = b.cl.DeleteApp(ctx, name)
		_ = b.cl.DeleteVolume(ctx, vol)
	}()

	running := func() bool {
		r, err := b.cl.GetApp(ctx, name)
		return err == nil && r.Status != nil && r.Status.Phase == "running"
	}
	if !until(60*time.Second, running) {
		fatal("volume app never reached running", fmt.Errorf("app %s", name))
	}

	// The daemon's own restore latency, collected alongside the wall-clock wake.
	var restore []int64
	wake := b.measure("volume wake (asleep → running)", func() time.Duration {
		if _, err := b.cl.SleepApp(ctx, name); err != nil {
			fatal("sleep volume app", err)
		}
		if !until(30*time.Second, func() bool {
			r, _ := b.cl.GetApp(ctx, name)
			return r.Status != nil && r.Status.Phase == "asleep"
		}) {
			fatal("volume app never reached asleep", fmt.Errorf("app %s", name))
		}
		t0 := time.Now()
		if _, err := b.cl.WakeApp(ctx, name); err != nil {
			fatal("wake volume app", err)
		}
		if !until(30*time.Second, running) {
			fatal("volume app never woke", fmt.Errorf("app %s", name))
		}
		d := time.Since(t0)
		if r, err := b.cl.GetApp(ctx, name); err == nil && r.Status != nil && r.Status.LastWakeLatencyMs > 0 {
			restore = append(restore, r.Status.LastWakeLatencyMs)
		}
		return d
	})

	// measure() runs b.warmup extra iterations first; drop that many restore
	// samples so the reported distribution matches the measured wakes.
	if len(restore) > b.warmup {
		restore = restore[b.warmup:]
	}
	res := map[string]any{"asleep_to_running_ms": wake, "image": image}
	if len(restore) > 0 {
		sort.Slice(restore, func(i, j int) bool { return restore[i] < restore[j] })
		p := func(q float64) int64 { return restore[int(q*float64(len(restore)-1))] }
		res["daemon_restore_ms"] = map[string]any{"p50": p(0.50), "p90": p(0.90), "p99": p(0.99), "min": restore[0], "max": restore[len(restore)-1]}
		fmt.Printf("  %-24s p50 %s   %s\n", "  daemon restore",
			accent.Render(fmt.Sprintf("%dms", p(0.50))),
			dim.Render(fmt.Sprintf("(last_wake_latency_ms, min %dms, max %dms, n=%d)", restore[0], restore[len(restore)-1], len(restore))))
	}
	b.results["volume_wake"] = res
}
