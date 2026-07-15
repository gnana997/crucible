package app

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestLedger builds a usage ledger over a fresh store with an injectable
// clock (reusing the package's fakeClock).
func newTestLedger(t *testing.T) (*usageLedger, *fakeClock, *Store) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	return newUsageLedger(st, clk.now, nil), clk, st
}

const gib = int64(1) << 30

// usageToAPI converts internal integer sub-units (millis) to the public
// seconds-based shape; storage additionally converts MiB→GiB.
func TestUsageToAPIConversion(t *testing.T) {
	u := Usage{
		AppID: "app_a", AppName: "a",
		ComputeVCPUMillis: 7_200_000,     // 7200 s
		MemoryMiBMillis:   3_600_000,     // 3600 s
		StorageMiBMillis:  1024 * 60_000, // 1024 MiB · 60 s = 60 GiB·s
		Requests:          4,
		RequestsByCode:    map[string]uint64{"2xx": 3, "4xx": 1},
	}
	a := usageToAPI(u)
	if a.ComputeVCPUSeconds != 7200 || a.MemoryMiBSeconds != 3600 || a.StorageGiBSeconds != 60 {
		t.Fatalf("convert = compute %v mem %v storage %v; want 7200/3600/60",
			a.ComputeVCPUSeconds, a.MemoryMiBSeconds, a.StorageGiBSeconds)
	}
	if a.Requests != 4 || a.RequestsByCode["2xx"] != 3 {
		t.Fatalf("requests not carried: %d %v", a.Requests, a.RequestsByCode)
	}
}

// The first observe on a fresh app must not back-fill any elapsed time — it only
// seeds the state; accrual starts from there.
func TestUsageNoBackfillOnFirstObserve(t *testing.T) {
	led, clk, _ := newTestLedger(t)
	clk.advance(time.Hour) // time passes before the app exists
	led.observe("app_a", "a", true, 2, 512, 0)
	if u := led.Snapshot("app_a", "a"); u.ComputeVCPUMillis != 0 || u.MemoryMiBMillis != 0 {
		t.Fatalf("first observe back-filled: compute=%d mem=%d, want 0/0", u.ComputeVCPUMillis, u.MemoryMiBMillis)
	}
	clk.advance(10 * time.Second)
	u := led.Snapshot("app_a", "a")
	if u.ComputeVCPUMillis != 2*10_000 || u.MemoryMiBMillis != 512*10_000 {
		t.Fatalf("compute=%d mem=%d, want %d/%d", u.ComputeVCPUMillis, u.MemoryMiBMillis, 2*10_000, 512*10_000)
	}
}

// Compute/memory accrue only while awake; a sleep freezes them and a later wake
// resumes — the core "compute freezes while asleep" property.
func TestUsageComputeFreezesWhileAsleep(t *testing.T) {
	led, clk, _ := newTestLedger(t)
	led.observe("app_a", "a", true, 2, 512, 0) // seed (awake)
	clk.advance(10 * time.Second)
	led.observe("app_a", "a", false, 2, 512, 0) // sleep at t+10 → accrues the 10s
	frozen := led.Snapshot("app_a", "a").ComputeVCPUMillis
	if frozen != 2*10_000 {
		t.Fatalf("at sleep compute=%d, want %d", frozen, 2*10_000)
	}
	clk.advance(30 * time.Second) // asleep — no compute
	if got := led.Snapshot("app_a", "a").ComputeVCPUMillis; got != frozen {
		t.Fatalf("compute accrued while asleep: %d, want frozen %d", got, frozen)
	}
	led.observe("app_a", "a", true, 2, 512, 0) // wake at t+40
	clk.advance(5 * time.Second)
	if got := led.Snapshot("app_a", "a").ComputeVCPUMillis; got != frozen+2*5_000 {
		t.Fatalf("after wake compute=%d, want %d", got, frozen+2*5_000)
	}
}

// Storage accrues whether the app is awake or asleep — a slept app still holds
// its disk.
func TestUsageStorageAccruesWhileAsleep(t *testing.T) {
	led, clk, _ := newTestLedger(t)
	led.observe("app_a", "a", false, 2, 512, gib) // asleep, 1 GiB volume
	clk.advance(10 * time.Second)
	u := led.Snapshot("app_a", "a")
	if u.ComputeVCPUMillis != 0 {
		t.Fatalf("asleep compute=%d, want 0", u.ComputeVCPUMillis)
	}
	if u.StorageMiBMillis != 1024*10_000 { // 1 GiB = 1024 MiB, 10 s
		t.Fatalf("storage=%d, want %d", u.StorageMiBMillis, 1024*10_000)
	}
}

// A spec change (e.g. redeploy with more vCPUs) accrues the prior interval with
// the OLD dims, then the new dims apply going forward.
func TestUsageDimChangeAppliesForward(t *testing.T) {
	led, clk, _ := newTestLedger(t)
	led.observe("app_a", "a", true, 2, 256, 0)
	clk.advance(10 * time.Second)
	led.observe("app_a", "a", true, 4, 256, 0) // scale up at t+10
	clk.advance(10 * time.Second)
	u := led.Snapshot("app_a", "a")
	// 10 s @ 2 vCPU + 10 s @ 4 vCPU = 20000 + 40000.
	if u.ComputeVCPUMillis != 2*10_000+4*10_000 {
		t.Fatalf("compute=%d, want %d", u.ComputeVCPUMillis, 2*10_000+4*10_000)
	}
}

func TestUsageRequests(t *testing.T) {
	led, _, _ := newTestLedger(t)
	for i := 0; i < 3; i++ {
		led.AddRequest("app_a", "a", "2xx")
	}
	led.AddRequest("app_a", "a", "4xx")
	u := led.Snapshot("app_a", "a")
	if u.Requests != 4 || u.RequestsByCode["2xx"] != 3 || u.RequestsByCode["4xx"] != 1 {
		t.Fatalf("requests=%d byCode=%v, want 4 {2xx:3,4xx:1}", u.Requests, u.RequestsByCode)
	}
}

// Counters persist across a daemon restart (a new ledger over the same store),
// and the post-restart observe does NOT back-fill the downtime.
func TestUsageDurableAcrossRestart(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}

	led1 := newUsageLedger(st, clk.now, nil)
	led1.observe("app_a", "a", true, 2, 512, 0)
	clk.advance(10 * time.Second)
	led1.AddRequest("app_a", "a", "2xx")
	led1.observe("app_a", "a", true, 2, 512, 0) // flush accrual to store
	want := led1.Snapshot("app_a", "a").ComputeVCPUMillis
	if want != 2*10_000 {
		t.Fatalf("pre-restart compute=%d, want %d", want, 2*10_000)
	}

	// "restart": downtime passes, a new ledger loads from the same store.
	clk.advance(time.Hour)
	led2 := newUsageLedger(st, clk.now, nil)
	u := led2.Snapshot("app_a", "a")
	if u.ComputeVCPUMillis != want || u.Requests != 1 {
		t.Fatalf("post-restart compute=%d req=%d, want %d/1 (downtime must not back-fill)", u.ComputeVCPUMillis, u.Requests, want)
	}
	// Resume awake accrual for 5 s — only the new interval counts.
	led2.observe("app_a", "a", true, 2, 512, 0)
	clk.advance(5 * time.Second)
	if got := led2.Snapshot("app_a", "a").ComputeVCPUMillis; got != want+2*5_000 {
		t.Fatalf("post-restart accrual=%d, want %d", got, want+2*5_000)
	}
}

// Finalize records a final accrual, marks the record finalized, and retains it
// in the store (so a control plane can read a deleted app's usage), while
// dropping the in-memory accum.
func TestUsageFinalizeRetains(t *testing.T) {
	led, clk, st := newTestLedger(t)
	led.observe("app_a", "a", true, 2, 512, 0)
	clk.advance(10 * time.Second)
	led.Finalize("app_a", "a")

	u, found, err := st.GetUsage("app_a")
	if err != nil || !found {
		t.Fatalf("GetUsage after finalize: found=%v err=%v", found, err)
	}
	if u.FinalizedAt == nil {
		t.Error("FinalizedAt not set")
	}
	if u.ComputeVCPUMillis != 2*10_000 {
		t.Errorf("final compute=%d, want %d", u.ComputeVCPUMillis, 2*10_000)
	}
	if _, ok := led.accum["app_a"]; ok {
		t.Error("accum not dropped after finalize")
	}
}
