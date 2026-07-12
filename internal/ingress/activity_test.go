package ingress

import (
	"testing"
	"time"
)

func TestActivityTracker(t *testing.T) {
	tr := NewActivityTracker()
	base := time.Unix(1_700_000_000, 0)
	clk := base
	tr.now = func() time.Time { return clk }

	// Unseen app → ok false (idle monitor leaves it alone).
	if _, _, ok := tr.Activity("web"); ok {
		t.Fatal("unseen app reported ok=true")
	}

	// begin: inflight 1, last = now.
	tr.begin("web")
	if last, inflight, ok := tr.Activity("web"); !ok || inflight != 1 || !last.Equal(base) {
		t.Fatalf("after begin: last=%v inflight=%d ok=%v", last, inflight, ok)
	}

	// A second concurrent request.
	clk = base.Add(2 * time.Second)
	tr.begin("web")
	if _, inflight, _ := tr.Activity("web"); inflight != 2 {
		t.Fatalf("after 2nd begin: inflight=%d, want 2", inflight)
	}

	// end: inflight drops, last advances to the end time (long request's
	// completion also counts as activity).
	clk = base.Add(5 * time.Second)
	tr.end("web")
	tr.end("web")
	last, inflight, _ := tr.Activity("web")
	if inflight != 0 {
		t.Fatalf("after two ends: inflight=%d, want 0", inflight)
	}
	if !last.Equal(base.Add(5 * time.Second)) {
		t.Fatalf("last=%v, want the end time", last)
	}

	// Underflow guard: an extra end doesn't push inflight negative.
	tr.end("web")
	if _, inflight, _ := tr.Activity("web"); inflight != 0 {
		t.Fatalf("inflight went negative: %d", inflight)
	}
}
