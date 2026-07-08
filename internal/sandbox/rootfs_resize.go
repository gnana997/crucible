package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// rootfsResizeTimeout bounds the resize2fs shell-out so a wedged tool can't
// hang a create forever. Growing an ext4 offline is fast (seconds).
const rootfsResizeTimeout = 2 * time.Minute

// growRootfs grows the ext4 image at path to at least sizeBytes: it enlarges
// the file (sparse truncate) and then resize2fs's the filesystem to fill it.
//
// It never shrinks — the per-sandbox clone already carries the image/profile
// content plus its built-in headroom, so a requested size at or below the
// current file is a no-op (returns nil). Operates on the unmounted clone
// before boot; the shared template is never touched.
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
	if err := os.Truncate(path, sizeBytes); err != nil {
		return fmt.Errorf("sandbox: grow rootfs file to %d bytes: %w", sizeBytes, err)
	}

	ctx, cancel := context.WithTimeout(ctx, rootfsResizeTimeout)
	defer cancel()
	// resize2fs with no size argument grows the filesystem to fill the
	// (now larger) file. -f forces the resize even if resize2fs thinks it's
	// unnecessary; the clone is a pristine, never-mounted ext4 so no e2fsck
	// is needed first.
	out, err := exec.CommandContext(ctx, "resize2fs", "-f", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sandbox: resize2fs %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
