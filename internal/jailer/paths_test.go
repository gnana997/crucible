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
