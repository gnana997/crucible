package jailer

import (
	"reflect"
	"testing"
)

// baseSpec is the minimum valid Spec for argv tests. Tests clone it
// and mutate fields rather than rebuilding from scratch.
func baseSpec() Spec {
	return Spec{
		ID:         "sbx-abc",
		ExecFile:   "/usr/bin/firecracker",
		UID:        10000,
		GID:        10000,
		ChrootBase: "/srv/jailer",
	}
}

func TestBuildArgsMinimal(t *testing.T) {
	got := BuildArgs(baseSpec(), nil)
	want := []string{
		"--id", "sbx-abc",
		"--exec-file", "/usr/bin/firecracker",
		"--uid", "10000",
		"--gid", "10000",
		"--chroot-base-dir", "/srv/jailer",
		"--cgroup-version", "2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgsAllQuotas(t *testing.T) {
	s := baseSpec()
	s.Quotas = Quotas{
		CPUMax:         "20000 100000",
		MemoryMaxBytes: 536870912, // 512 MiB
		PIDsMax:        100,
	}
	got := BuildArgs(s, nil)
	want := []string{
		"--id", "sbx-abc",
		"--exec-file", "/usr/bin/firecracker",
		"--uid", "10000",
		"--gid", "10000",
		"--chroot-base-dir", "/srv/jailer",
		"--cgroup-version", "2",
		"--cgroup", "cpu.max=20000 100000",
		"--cgroup", "memory.max=536870912",
		"--cgroup", "pids.max=100",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgsOnlyCPUQuota(t *testing.T) {
	s := baseSpec()
	s.Quotas.CPUMax = "50000 100000"
	got := BuildArgs(s, nil)
	for i, a := range got {
		if a == "memory.max" {
			t.Fatalf("memory.max flag should not be present: %v", got)
		}
		if a == "pids.max" {
			t.Fatalf("pids.max flag should not be present: %v", got)
		}
		_ = i
	}
	// Spot check the positive: cpu.max=... is present.
	found := false
	for _, a := range got {
		if a == "cpu.max=50000 100000" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected cpu.max=50000 100000 in argv: %v", got)
	}
}

func TestBuildArgsZeroQuotasOmitted(t *testing.T) {
	s := baseSpec()
	s.Quotas = Quotas{} // explicit zero
	got := BuildArgs(s, nil)
	for _, a := range got {
		if a == "--cgroup" {
			t.Fatalf("zero-valued quotas should not emit --cgroup: %v", got)
		}
	}
}

func TestBuildArgsNewPIDNS(t *testing.T) {
	s := baseSpec()
	s.NewPIDNS = true
	got := BuildArgs(s, nil)
	found := false
	for _, a := range got {
		if a == "--new-pid-ns" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --new-pid-ns in argv: %v", got)
	}
}

func TestBuildArgsForwardsFcArgs(t *testing.T) {
	s := baseSpec()
	got := BuildArgs(s, []string{"--log-path", "/var/log/fc.log"})
	// Find the -- separator and make sure fcArgs follow it.
	sepIdx := -1
	for i, a := range got {
		if a == "--" {
			sepIdx = i
			break
		}
	}
	if sepIdx == -1 {
		t.Fatalf("expected -- separator when fcArgs present: %v", got)
	}
	tail := got[sepIdx+1:]
	want := []string{"--log-path", "/var/log/fc.log"}
	if !reflect.DeepEqual(tail, want) {
		t.Fatalf("fcArgs tail mismatch\n got: %v\nwant: %v", tail, want)
	}
}

func TestBuildArgsNoSeparatorWhenFcArgsEmpty(t *testing.T) {
	s := baseSpec()
	got := BuildArgs(s, nil)
	for _, a := range got {
		if a == "--" {
			t.Fatalf("unexpected -- separator with empty fcArgs: %v", got)
		}
	}
}

func TestBuildArgsLargeUIDGID(t *testing.T) {
	s := baseSpec()
	s.UID = 4294967290 // near uint32 max
	s.GID = 4294967291
	got := BuildArgs(s, nil)
	sawUID, sawGID := false, false
	for i, a := range got {
		if a == "--uid" && i+1 < len(got) && got[i+1] == "4294967290" {
			sawUID = true
		}
		if a == "--gid" && i+1 < len(got) && got[i+1] == "4294967291" {
			sawGID = true
		}
	}
	if !sawUID || !sawGID {
		t.Fatalf("uid/gid not rendered correctly: %v", got)
	}
}
