package sandbox

import (
	"testing"

	"github.com/gnana997/crucible/internal/runner"
)

func TestDeriveQuotas(t *testing.T) {
	const mib = int64(1) << 20
	tests := []struct {
		name       string
		vcpus      int
		memMiB     int
		wantCPU    string
		wantMemMiB int64 // expected memory.max in MiB
	}{
		{"1cpu-256mib-headroom-floor", 1, 256, "100000 100000", 256 + 128}, // 25% = 64 < 128 floor
		{"2cpu-1024mib", 2, 1024, "200000 100000", 1024 + 256},             // 25% = 256
		{"4cpu-2048mib", 4, 2048, "400000 100000", 2048 + 512},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveQuotas(tt.vcpus, tt.memMiB)
			if got.CPUMax != tt.wantCPU {
				t.Errorf("CPUMax = %q, want %q", got.CPUMax, tt.wantCPU)
			}
			if want := tt.wantMemMiB * mib; got.MemoryMaxBytes != want {
				t.Errorf("MemoryMaxBytes = %d, want %d (%d MiB)", got.MemoryMaxBytes, want, tt.wantMemMiB)
			}
			if got.PIDsMax != defaultVMMPidsMax {
				t.Errorf("PIDsMax = %d, want %d", got.PIDsMax, defaultVMMPidsMax)
			}
		})
	}
}

func TestQuotasForRespectsPolicy(t *testing.T) {
	// Off (the zero value) applies no limits so BuildArgs omits every
	// --cgroup flag; Derive sizes them from the request.
	off := &Manager{cfg: ManagerConfig{QuotaPolicy: QuotaPolicyOff}}
	if got := off.quotasFor(2, 1024); got != (runner.Quotas{}) {
		t.Errorf("QuotaPolicyOff quotasFor = %+v, want zero", got)
	}

	derive := &Manager{cfg: ManagerConfig{QuotaPolicy: QuotaPolicyDerive}}
	if got, want := derive.quotasFor(2, 1024), deriveQuotas(2, 1024); got != want {
		t.Errorf("QuotaPolicyDerive quotasFor = %+v, want %+v", got, want)
	}

	// Zero value of ManagerConfig means off (today's default for
	// library callers and tests).
	zero := &Manager{}
	if got := zero.quotasFor(2, 1024); got != (runner.Quotas{}) {
		t.Errorf("zero-value policy quotasFor = %+v, want zero", got)
	}
}
