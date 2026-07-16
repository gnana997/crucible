package sandbox

import (
	"context"
	"fmt"
	"os"

	"github.com/gnana997/crucible/internal/fsutil"
)

// growRootfs grows the ext4 image at path to at least sizeBytes: it enlarges
// the file (sparse truncate) and then resize2fs's the filesystem to fill it.
//
// It never shrinks — the per-sandbox clone already carries the image/profile
// content plus its built-in headroom, so a requested size at or below the
// current file is a no-op (returns nil, skipping the resize2fs shell-out).
// Operates on the unmounted clone before boot; the shared template is never
// touched. The truncate+resize2fs mechanism is shared with volume grow via
// fsutil.GrowExt4.
//
// Requires resize2fs (e2fsprogs) on the host, the same dependency the OCI
// conversion's mkfs.ext4 already assumes.
func growRootfs(ctx context.Context, path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("sandbox: stat rootfs clone: %w", err)
	}
	if sizeBytes <= fi.Size() {
		// The clone is already at least this large — keep its headroom.
		return nil
	}
	return fsutil.GrowExt4(ctx, path, sizeBytes)
}
