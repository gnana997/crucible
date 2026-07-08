package oci

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireTool skips the test if an external tool is absent.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not installed; skipping materialize test", name)
	}
}

func debugfsLs(t *testing.T, imgPath, dir string) string {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", "ls -l "+dir, imgPath).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs ls %s: %v: %s", dir, err, out)
	}
	return string(out)
}

func debugfsStatT(t *testing.T, imgPath, path string) string {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", "stat "+path, imgPath).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs stat %s: %v: %s", path, err, out)
	}
	s := string(out)
	if strings.Contains(s, "File not found") {
		t.Fatalf("debugfs: %s not found in image", path)
	}
	return s
}

// materializeModes returns the modes worth testing on this host:
// staging always, pipe only where mkfs supports tarballs.
func materializeModes(t *testing.T) []MaterializeMode {
	t.Helper()
	modes := []MaterializeMode{ModeStaging}
	if ProbeTarballSupport(t.Context()) {
		modes = append([]MaterializeMode{ModePipe}, modes...)
	} else {
		t.Log("host mkfs lacks tarball support; pipe-mode cases skipped")
	}
	return modes
}

func TestProbeTarballSupport(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")
	// Just exercise the probe path; the result is host-dependent.
	_ = ProbeTarballSupport(t.Context())
}

func TestMaterializeRoundTrip(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "fsck.ext4")
	requireTool(t, "debugfs")

	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/app", mode: 0o755, content: "#!ELF app"},
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/config", mode: 0o644, content: "key=value"},
		{name: "etc/link", typeflag: tar.TypeSymlink, link: "config"},
	}))

	for _, mode := range materializeModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "rootfs.ext4")
			res, err := Materialize(t.Context(), acq, dest, MaterializeOptions{
				AssembleOptions: AssembleOptions{Agent: []byte("FAKE-AGENT"), Now: fixedNow},
				Mode:            mode,
			})
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			if res.Mode != mode.String() {
				t.Errorf("result mode = %q, want %q", res.Mode, mode.String())
			}
			if res.SizeBytes < sizePadFloorBytes {
				t.Errorf("size %d below floor", res.SizeBytes)
			}
			fi, err := os.Stat(dest)
			if err != nil || fi.Size() != res.SizeBytes {
				t.Fatalf("image stat: %v (size %d, want %d)", err, fi.Size(), res.SizeBytes)
			}

			// Injected artifacts present (validateImage already checked,
			// but assert the layout independently).
			ls := debugfsLs(t, dest, "/crucible")
			if !strings.Contains(ls, "crucible-agent") || !strings.Contains(ls, "run.json") {
				t.Errorf("/crucible missing injected files: %s", ls)
			}
			// Image content present.
			if st := debugfsStatT(t, dest, "/bin/app"); !strings.Contains(st, "Inode:") {
				t.Errorf("bin/app missing: %s", st)
			}
			debugfsStatT(t, dest, "/etc/config")
			// Symlink preserved as a symlink.
			if st := debugfsStatT(t, dest, "/etc/link"); !strings.Contains(st, "symlink") && !strings.Contains(st, "Fast link dest") {
				t.Errorf("etc/link not a symlink: %s", st)
			}
		})
	}
}

func TestMaterializeSetuidPreserved(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "usr/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/sudo", mode: 0o4755, content: "fake-sudo"},
	}))

	for _, mode := range materializeModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			if mode == ModeStaging && os.Geteuid() != 0 {
				t.Skip("staging setuid fidelity needs root (chmod of a foreign-owned tree)")
			}
			dest := filepath.Join(t.TempDir(), "rootfs.ext4")
			if _, err := Materialize(t.Context(), acq, dest, MaterializeOptions{
				AssembleOptions: AssembleOptions{Agent: []byte("A"), Now: fixedNow},
				Mode:            mode,
			}); err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			st := debugfsStatT(t, dest, "/usr/bin/sudo")
			// debugfs prints the mode as e.g. "0104755"; the 4 in the
			// suid position is what matters.
			if !strings.Contains(st, "4755") {
				t.Errorf("setuid bit lost in image: %s", firstLine(st))
			}
		})
	}
}

func TestMaterializeUidGidPreserved(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "data", mode: 0o644, uid: 1000, gid: 1000, content: "owned"},
	}))
	for _, mode := range materializeModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			if mode == ModeStaging && os.Geteuid() != 0 {
				t.Skip("staging uid/gid fidelity needs root")
			}
			dest := filepath.Join(t.TempDir(), "rootfs.ext4")
			if _, err := Materialize(t.Context(), acq, dest, MaterializeOptions{
				AssembleOptions: AssembleOptions{Agent: []byte("A"), Now: fixedNow},
				Mode:            mode,
			}); err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			st := debugfsStatT(t, dest, "/data")
			if !strings.Contains(st, "1000") {
				t.Errorf("uid/gid 1000 not preserved: %s", firstLine(st))
			}
		})
	}
}

func TestMaterializeManyEntriesInodeSizing(t *testing.T) {
	requireTool(t, "mkfs.ext4")
	requireTool(t, "debugfs")

	// Many tiny files would exhaust the default inode budget if we
	// didn't compute -N. Small enough to stay fast.
	entries := []tarEntry{{name: "files/", typeflag: tar.TypeDir, mode: 0o755}}
	for i := 0; i < 3000; i++ {
		entries = append(entries, tarEntry{name: "files/f" + itoaT(i), mode: 0o644, content: "x"})
	}
	acq := acquiredFromLayers(t, layerOf(t, entries))

	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	res, err := Materialize(t.Context(), acq, dest, MaterializeOptions{
		AssembleOptions: AssembleOptions{Agent: []byte("A"), Now: fixedNow},
		Mode:            materializeModes(t)[0],
	})
	if err != nil {
		t.Fatalf("Materialize with many entries: %v", err)
	}
	if res.Stats.Entries < 3000 {
		t.Errorf("entries = %d, want >= 3000", res.Stats.Entries)
	}
	debugfsStatT(t, dest, "/files/f2999")
}

func TestMaterializeLeavesDestUntouchedOnAssembleError(t *testing.T) {
	// No agent → Assemble fails → dest must not appear, scratch cleaned.
	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{{name: "a", content: "x"}}))
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if _, err := Materialize(t.Context(), acq, dest, MaterializeOptions{Mode: ModePipe}); err == nil {
		t.Fatal("Materialize without an agent succeeded")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("dest exists after a failed conversion: %v", err)
	}
	// The scratch temp dir under dest's parent must be gone.
	entries, _ := os.ReadDir(filepath.Dir(dest))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".crucible-convert-") {
			t.Errorf("scratch dir leaked: %s", e.Name())
		}
	}
}

func TestComputeImageSize(t *testing.T) {
	cases := []struct{ content, wantMin int64 }{
		{content: 0, wantMin: sizePadFloorBytes},
		{content: 100 << 20, wantMin: 100<<20 + sizePadFloorBytes}, // pad floored at 256MiB
		{content: 10 << 30, wantMin: 10<<30 + 10<<30/5},            // 20% pad dominates
	}
	for _, tc := range cases {
		got := computeImageSize(tc.content)
		if got < tc.wantMin {
			t.Errorf("computeImageSize(%d) = %d, want >= %d", tc.content, got, tc.wantMin)
		}
		if got%sizeAlign != 0 {
			t.Errorf("computeImageSize(%d) = %d not MiB-aligned", tc.content, got)
		}
	}
}

func TestInodeCount(t *testing.T) {
	// Few entries in a big image: accept the default (0).
	if n := inodeCount(10, 512<<20); n != 0 {
		t.Errorf("inodeCount(10, 512MiB) = %d, want 0 (default)", n)
	}
	// Many entries in a small image: override with a positive count.
	if n := inodeCount(200_000, 300<<20); n <= 0 {
		t.Errorf("inodeCount(200k, 300MiB) = %d, want positive override", n)
	}
}

func itoaT(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

var _ = context.Background
