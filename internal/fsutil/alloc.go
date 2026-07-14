package fsutil

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// AllocatedBytes returns the bytes a file actually occupies on disk
// (st_blocks × 512). For the sparse files crucible manages — snapshot memory
// files, volume backing files — this is the honest usage number, far below the
// logical size. Reflink-shared blocks are counted per file (each reflink
// reports its full allocation). A missing or unstatable path counts as 0:
// artifacts race with deletion at scrape time, and a gauge read must never
// error.
func AllocatedBytes(path string) int64 {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0
	}
	return st.Blocks * 512
}

// DirAllocatedBytes sums AllocatedBytes over a directory's immediate regular
// files (snapshot artifact dirs are flat: state + memory + rootfs). A missing
// directory counts as 0, same contract as AllocatedBytes.
func DirAllocatedBytes(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		total += AllocatedBytes(filepath.Join(dir, e.Name()))
	}
	return total
}
