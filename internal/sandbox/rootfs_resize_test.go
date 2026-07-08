package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
)

func TestGrowRootfsZeroIsNoop(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeFileOfSize(t, f, 4096)
	if err := growRootfs(context.Background(), f, 0); err != nil {
		t.Fatalf("growRootfs(0): %v", err)
	}
	if got := fileSize(t, f); got != 4096 {
		t.Errorf("size = %d, want unchanged 4096", got)
	}
}

func TestGrowRootfsSmallerIsNoop(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeFileOfSize(t, f, 1<<20) // 1 MiB
	// A requested size <= current must not shrink or invoke resize2fs.
	if err := growRootfs(context.Background(), f, 512<<10); err != nil {
		t.Fatalf("growRootfs(smaller): %v", err)
	}
	if got := fileSize(t, f); got != 1<<20 {
		t.Errorf("size = %d, want unchanged %d (never shrink)", got, 1<<20)
	}
}

// TestGrowRootfsResizesExt4 proves the real grow path: mkfs a small ext4,
// grow it, and confirm both the file AND the filesystem's block count grew.
func TestGrowRootfsResizesExt4(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("resize2fs is linux/e2fsprogs")
	}
	for _, tool := range []string{"mkfs.ext4", "resize2fs", "dumpe2fs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	f := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeFileOfSize(t, f, 16<<20) // 16 MiB
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", f).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}
	before := ext4BlockCount(t, f)

	const target = 64 << 20 // 64 MiB
	if err := growRootfs(context.Background(), f, target); err != nil {
		t.Fatalf("growRootfs: %v", err)
	}
	if got := fileSize(t, f); got != target {
		t.Errorf("file size = %d, want %d", got, int64(target))
	}
	after := ext4BlockCount(t, f)
	if after <= before {
		t.Errorf("ext4 block count did not grow: before=%d after=%d (resize2fs only touched the file?)", before, after)
	}
}

func writeFileOfSize(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return fi.Size()
}

var blockCountRE = regexp.MustCompile(`(?m)^Block count:\s+(\d+)`)

func ext4BlockCount(t *testing.T, path string) int64 {
	t.Helper()
	out, err := exec.Command("dumpe2fs", "-h", path).CombinedOutput()
	if err != nil {
		t.Fatalf("dumpe2fs: %v: %s", err, out)
	}
	m := blockCountRE.FindSubmatch(out)
	if m == nil {
		t.Fatalf("no Block count in dumpe2fs output:\n%s", out)
	}
	n, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		t.Fatalf("parse block count %q: %v", m[1], err)
	}
	return n
}
