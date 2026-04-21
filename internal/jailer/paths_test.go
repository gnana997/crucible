package jailer

import (
	"strings"
	"testing"
)

func TestChrootRoot(t *testing.T) {
	s := Spec{
		ID:         "sbx-abc",
		ExecFile:   "/usr/bin/firecracker",
		ChrootBase: "/srv/jailer",
	}
	got := ChrootRoot(s)
	want := "/srv/jailer/firecracker/sbx-abc/root"
	if got != want {
		t.Fatalf("ChrootRoot = %q, want %q", got, want)
	}
}

func TestChrootRootHandlesExecFileBasenameOnly(t *testing.T) {
	// filepath.Base on just "firecracker" should yield "firecracker".
	s := Spec{
		ID:         "x",
		ExecFile:   "firecracker",
		ChrootBase: "/srv/jailer",
	}
	if got := ChrootRoot(s); got != "/srv/jailer/firecracker/x/root" {
		t.Fatalf("ChrootRoot = %q", got)
	}
}

func TestHostPath(t *testing.T) {
	s := Spec{ID: "x", ExecFile: "/usr/bin/firecracker", ChrootBase: "/srv/jailer"}
	cases := []struct {
		chrootRel string
		want      string
	}{
		{"/v.sock", "/srv/jailer/firecracker/x/root/v.sock"},
		{"v.sock", "/srv/jailer/firecracker/x/root/v.sock"},
		{"///v.sock", "/srv/jailer/firecracker/x/root/v.sock"},
		{"/run/firecracker.socket", "/srv/jailer/firecracker/x/root/run/firecracker.socket"},
		{"/vmlinux", "/srv/jailer/firecracker/x/root/vmlinux"},
		{"/", "/srv/jailer/firecracker/x/root"},
	}
	for _, tc := range cases {
		got := HostPath(s, tc.chrootRel)
		if got != tc.want {
			t.Errorf("HostPath(%q) = %q, want %q", tc.chrootRel, got, tc.want)
		}
	}
}

func TestChrootRelRoundtrip(t *testing.T) {
	s := Spec{ID: "x", ExecFile: "/usr/bin/firecracker", ChrootBase: "/srv/jailer"}
	for _, chrootRel := range []string{"/v.sock", "/run/firecracker.socket", "/vmlinux", "/nested/deep/file"} {
		host := HostPath(s, chrootRel)
		got, err := ChrootRel(s, host)
		if err != nil {
			t.Fatalf("ChrootRel(%q) error: %v", host, err)
		}
		if got != chrootRel {
			t.Errorf("roundtrip: HostPath(%q) -> ChrootRel -> %q", chrootRel, got)
		}
	}
}

func TestChrootRelRejectsOutsidePath(t *testing.T) {
	s := Spec{ID: "x", ExecFile: "/usr/bin/firecracker", ChrootBase: "/srv/jailer"}
	_, err := ChrootRel(s, "/etc/passwd")
	if err == nil {
		t.Fatal("expected error for path outside chroot, got nil")
	}
	if !strings.Contains(err.Error(), "outside chroot") {
		t.Fatalf("error = %q, want 'outside chroot'", err.Error())
	}
}

func TestChrootRelAtChrootRoot(t *testing.T) {
	s := Spec{ID: "x", ExecFile: "/usr/bin/firecracker", ChrootBase: "/srv/jailer"}
	got, err := ChrootRel(s, ChrootRoot(s))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/" {
		t.Fatalf("ChrootRel of chroot root = %q, want %q", got, "/")
	}
}

func TestChrootDirIsParentOfChrootRoot(t *testing.T) {
	s := Spec{ID: "x", ExecFile: "/usr/bin/firecracker", ChrootBase: "/srv/jailer"}
	if got := ChrootDir(s); got != "/srv/jailer/firecracker/x" {
		t.Fatalf("ChrootDir = %q", got)
	}
}

// TestForkVsockIsolation pins the invariant that makes same-host fork
// work under Firecracker v1.15 without the (unreleased) vsock_override:
// the vsock UDS path (always "/v.sock" chroot-relative) must resolve
// to distinct absolute host paths for each VM. If this ever returned
// the same host path for two VMs, two firecracker processes would
// race to bind the same UDS and the second would fail with EADDRINUSE
// — exactly the bug jailer was adopted to fix.
func TestForkVsockIsolation(t *testing.T) {
	ids := []string{"fork-a", "fork-b", "fork-c"}
	seen := make(map[string]string)
	for _, id := range ids {
		s := Spec{ID: id, ExecFile: "/usr/bin/firecracker", ChrootBase: "/srv/jailer"}
		host := HostPath(s, "/v.sock")
		if prev, ok := seen[host]; ok {
			t.Fatalf("vsock host path collision: %s and %s both map to %s", prev, id, host)
		}
		seen[host] = id
	}
	if len(seen) != len(ids) {
		t.Fatalf("expected %d distinct host paths, got %d", len(ids), len(seen))
	}
}
