package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSleepPolicyJSONRoundTrip(t *testing.T) {
	in := AppSpec{
		Name:  "web",
		Image: &ImageRef{OCI: "nginx:alpine"},
		Sleep: &SleepPolicy{IdleTimeoutSec: 30, MinScale: 0},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// min_scale has no omitempty (0 is meaningful: scale-to-zero), so it must
	// always appear; idle_timeout_s omits when zero.
	if !strings.Contains(string(b), `"min_scale":0`) {
		t.Fatalf("min_scale not serialized: %s", b)
	}

	var out AppSpec
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Sleep == nil || out.Sleep.IdleTimeoutSec != 30 || out.Sleep.MinScale != 0 {
		t.Fatalf("round-trip lost sleep policy: %+v", out.Sleep)
	}

	// A spec with no sleep policy stays nil and emits no "sleep" key.
	b2, _ := json.Marshal(AppSpec{Name: "web", Image: &ImageRef{OCI: "x"}})
	if strings.Contains(string(b2), "sleep") {
		t.Fatalf("nil sleep policy leaked into JSON: %s", b2)
	}
}

func TestAppStatusSleepFieldsOmitEmpty(t *testing.T) {
	b, _ := json.Marshal(AppStatus{Phase: "running"})
	if strings.Contains(string(b), "last_wake_latency_ms") || strings.Contains(string(b), "sleep_count") {
		t.Fatalf("zero sleep status fields should omit: %s", b)
	}
	b2, _ := json.Marshal(AppStatus{Phase: "asleep", SleepCount: 2, LastWakeLatencyMs: 450})
	if !strings.Contains(string(b2), `"sleep_count":2`) || !strings.Contains(string(b2), `"last_wake_latency_ms":450`) {
		t.Fatalf("populated sleep status fields missing: %s", b2)
	}
}
