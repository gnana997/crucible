package daemon

import (
	"testing"

	"github.com/gnana997/crucible/internal/oci"
)

func TestHealthFromImage(t *testing.T) {
	if healthFromImage(nil) != nil {
		t.Error("nil image healthcheck should seed nothing")
	}
	if healthFromImage(&oci.Healthcheck{Test: []string{"NONE"}}) != nil {
		t.Error("NONE should seed nothing (explicitly disabled)")
	}
	if healthFromImage(&oci.Healthcheck{Test: []string{"WEIRD", "x"}}) != nil {
		t.Error("unrecognized form should seed nothing")
	}

	// CMD (exec form) → argv verbatim; ms→s field mapping.
	h := healthFromImage(&oci.Healthcheck{
		Test:       []string{"CMD", "curl", "-f", "http://localhost/"},
		IntervalMs: 5000, TimeoutMs: 2000, StartPeriodMs: 10000, Retries: 3,
	})
	if h == nil || h.Type != "exec" || len(h.Cmd) != 3 || h.Cmd[0] != "curl" {
		t.Fatalf("CMD form wrong: %+v", h)
	}
	if h.IntervalSec != 5 || h.TimeoutSec != 2 || h.StartPeriodSec != 10 || h.UnhealthyThreshold != 3 {
		t.Errorf("field mapping wrong: %+v", h)
	}

	// CMD-SHELL → /bin/sh -c "<script>".
	h = healthFromImage(&oci.Healthcheck{Test: []string{"CMD-SHELL", "pg_isready || exit 1"}})
	if h == nil || h.Type != "exec" || len(h.Cmd) != 3 ||
		h.Cmd[0] != "/bin/sh" || h.Cmd[1] != "-c" || h.Cmd[2] != "pg_isready || exit 1" {
		t.Fatalf("CMD-SHELL form wrong: %+v", h)
	}
}
