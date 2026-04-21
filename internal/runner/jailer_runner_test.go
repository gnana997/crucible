package runner

import (
	"errors"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/jailer"
)

func TestSanitizeJailerID(t *testing.T) {
	cases := map[string]string{
		"sbx_abc123":        "sbx-abc123",
		"snap_l87p5qcl":     "snap-l87p5qcl",
		"sbx_foo_bar":       "sbx-foo-bar", // multiple underscores OK
		"already-sanitized": "already-sanitized",
		"":                  "",
	}
	for in, want := range cases {
		if got := sanitizeJailerID(in); got != want {
			t.Errorf("sanitizeJailerID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewJailerRunnerDefaultsUIDGID(t *testing.T) {
	// UID/GID of 0 → replaced with 10000; never keep root as the
	// drop target (using uid=0 would defeat the point of jailer).
	jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 0, 0)
	if jr.UID != 10000 || jr.GID != 10000 {
		t.Fatalf("defaults not applied: UID=%d GID=%d", jr.UID, jr.GID)
	}
}

func TestNewJailerRunnerRespectsNonZeroUIDGID(t *testing.T) {
	jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 1234, 5678)
	if jr.UID != 1234 || jr.GID != 5678 {
		t.Fatalf("explicit uid/gid lost: UID=%d GID=%d", jr.UID, jr.GID)
	}
}

func TestJailerValidateSelf(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*JailerRunner)
		wantErr string
	}{
		{name: "valid", mutate: func(*JailerRunner) {}},
		{
			name:    "no jailer bin",
			mutate:  func(j *JailerRunner) { j.JailerBin = "" },
			wantErr: "JailerBin is empty",
		},
		{
			name:    "no firecracker bin",
			mutate:  func(j *JailerRunner) { j.FirecrackerBin = "" },
			wantErr: "FirecrackerBin is empty",
		},
		{
			name:    "no chroot base",
			mutate:  func(j *JailerRunner) { j.ChrootBase = "" },
			wantErr: "ChrootBase is empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 10000, 10000)
			tc.mutate(jr)
			err := jr.validateSelf()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q missing %q", err.Error(), tc.wantErr)
			}
			if tc.wantErr != "" && err != nil && !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error %v does not wrap ErrInvalidSpec", err)
			}
		})
	}
}

func TestJailerValidateStart(t *testing.T) {
	jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 10000, 10000)
	base := Spec{
		Workdir:   "/tmp/sandboxes/sbx_abc",
		Kernel:    "/var/lib/crucible/vmlinux",
		Rootfs:    "/var/lib/crucible/rootfs.ext4",
		VCPUs:     2,
		MemoryMiB: 512,
	}
	cases := []struct {
		name    string
		mutate  func(*Spec)
		wantErr bool
	}{
		{name: "valid", mutate: func(*Spec) {}, wantErr: false},
		{name: "no workdir", mutate: func(s *Spec) { s.Workdir = "" }, wantErr: true},
		{name: "no kernel", mutate: func(s *Spec) { s.Kernel = "" }, wantErr: true},
		{name: "no rootfs", mutate: func(s *Spec) { s.Rootfs = "" }, wantErr: true},
		{name: "zero vcpus", mutate: func(s *Spec) { s.VCPUs = 0 }, wantErr: true},
		{name: "zero memory", mutate: func(s *Spec) { s.MemoryMiB = 0 }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			err := jr.validateStart(s)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected: %v", err)
			}
		})
	}
}

func TestJailerValidateRestore(t *testing.T) {
	jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 10000, 10000)
	base := RestoreSpec{
		Workdir:    "/tmp/sandboxes/sbx_fork",
		StatePath:  "/snap/state",
		MemPath:    "/snap/mem",
		RootfsPath: "/fork/rootfs",
	}
	cases := []struct {
		name    string
		mutate  func(*RestoreSpec)
		wantErr bool
	}{
		{name: "valid", mutate: func(*RestoreSpec) {}, wantErr: false},
		{name: "no workdir", mutate: func(s *RestoreSpec) { s.Workdir = "" }, wantErr: true},
		{name: "no state", mutate: func(s *RestoreSpec) { s.StatePath = "" }, wantErr: true},
		{name: "no mem", mutate: func(s *RestoreSpec) { s.MemPath = "" }, wantErr: true},
		{name: "no rootfs", mutate: func(s *RestoreSpec) { s.RootfsPath = "" }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			err := jr.validateRestore(s)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected: %v", err)
			}
		})
	}
}

func TestJailerBuildSpecSanitizesID(t *testing.T) {
	jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 10000, 10000)
	js, err := jr.buildJailerSpec("/var/run/crucible/sbx_abc123", Quotas{})
	if err != nil {
		t.Fatalf("buildJailerSpec: %v", err)
	}
	if js.ID != "sbx-abc123" {
		t.Fatalf("ID = %q, want %q (underscore sanitized)", js.ID, "sbx-abc123")
	}
	if js.ExecFile != "/usr/bin/firecracker" {
		t.Fatalf("ExecFile = %q", js.ExecFile)
	}
	if js.ChrootBase != "/srv/jailer" {
		t.Fatalf("ChrootBase = %q", js.ChrootBase)
	}
	if js.UID != 10000 || js.GID != 10000 {
		t.Fatalf("UID/GID: %d/%d", js.UID, js.GID)
	}
	if !js.NewPIDNS {
		t.Fatal("NewPIDNS should default to true")
	}
}

func TestJailerBuildSpecPlumbsQuotas(t *testing.T) {
	jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 10000, 10000)
	q := Quotas{
		CPUMax:         "50000 100000",
		MemoryMaxBytes: 1 << 30, // 1 GiB
		PIDsMax:        256,
	}
	js, err := jr.buildJailerSpec("/var/run/crucible/sbx_x", q)
	if err != nil {
		t.Fatalf("buildJailerSpec: %v", err)
	}
	want := jailer.Quotas{CPUMax: "50000 100000", MemoryMaxBytes: 1 << 30, PIDsMax: 256}
	if js.Quotas != want {
		t.Fatalf("Quotas = %+v, want %+v", js.Quotas, want)
	}
}

func TestJailerBuildSpecDisablePIDNS(t *testing.T) {
	jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 10000, 10000)
	jr.DisablePIDNS = true
	js, err := jr.buildJailerSpec("/var/run/crucible/sbx_x", Quotas{})
	if err != nil {
		t.Fatalf("buildJailerSpec: %v", err)
	}
	if js.NewPIDNS {
		t.Fatal("NewPIDNS should be false when DisablePIDNS is set")
	}
}

func TestJailerBuildSpecRejectsInvalidID(t *testing.T) {
	jr := NewJailerRunner("/usr/bin/jailer", "/usr/bin/firecracker", "/srv/jailer", 10000, 10000)
	// Empty workdir → empty ID → jailer.Spec.Validate fails.
	_, err := jr.buildJailerSpec("", Quotas{})
	if err == nil {
		t.Fatal("expected error for empty workdir producing invalid jailer ID")
	}
}

func TestJailerChrootRelativeConstants(t *testing.T) {
	// Sanity: the paths firecracker will see inside the chroot must
	// start with "/" (pivot_root semantics) and match what Stage
	// places there. This test catches accidental divergence if
	// someone adds a new path and forgets to Stage it.
	paths := []string{
		chrootKernelPath, chrootRootfsPath, chrootVsockPath,
		chrootStatePath, chrootMemPath, chrootAPISocketPath,
	}
	for _, p := range paths {
		if len(p) == 0 || p[0] != '/' {
			t.Errorf("chroot-relative path %q must start with /", p)
		}
	}
}
