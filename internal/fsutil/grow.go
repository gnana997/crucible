package fsutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// growExt4Timeout bounds each e2fsprogs shell-out so a wedged tool can't hang a
// caller forever. Growing an ext4 offline is fast (seconds).
const growExt4Timeout = 2 * time.Minute

// GrowExt4 grows the ext4 filesystem at path to fill its backing store, never
// shrinking. When sizeBytes > 0, path is treated as a regular FILE and is
// sparse-truncated up to sizeBytes first (a value at or below the current file
// size skips the truncate). Pass sizeBytes <= 0 when path is a block device
// already sized by its container (e.g. a decrypted LUKS mapper) to run resize2fs
// alone. Requires resize2fs (e2fsprogs) on the host — the same dependency the OCI
// conversion and volume mkfs already assume.
func GrowExt4(ctx context.Context, path string, sizeBytes int64) error {
	if sizeBytes > 0 {
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("fsutil: stat %s: %w", path, err)
		}
		if sizeBytes > fi.Size() {
			if err := os.Truncate(path, sizeBytes); err != nil {
				return fmt.Errorf("fsutil: grow %s to %d bytes: %w", path, sizeBytes, err)
			}
		}
	}
	// resize2fs refuses to grow a filesystem that was not cleanly unmounted — a
	// volume detached from a hard-killed guest carries an unreplayed journal.
	// e2fsck -f -p replays it and marks the fs clean (a pristine rootfs clone is
	// already clean, so this is a fast no-op there).
	if err := fsckExt4(ctx, path); err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, growExt4Timeout)
	defer cancel()
	// resize2fs with no size argument grows the filesystem to fill its device or
	// file. -f forces it even when resize2fs thinks nothing is needed.
	out, err := exec.CommandContext(rctx, "resize2fs", "-f", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("fsutil: resize2fs %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fsckExt4 runs e2fsck -f -p (preen: auto-fix safe issues, replay the journal)
// so a following resize2fs will proceed. e2fsck's exit status is a bitmask:
// 0 = clean, 1 = errors corrected, 2 = corrected + reboot advised — all success
// for our offline resize; >= 4 means uncorrected errors or an operational fault.
func fsckExt4(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, growExt4Timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "e2fsck", "-f", "-p", path).CombinedOutput()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() < 4 {
		return nil // errors corrected (exit 1/2) — safe to resize
	}
	return fmt.Errorf("fsutil: e2fsck %s: %w: %s", path, err, strings.TrimSpace(string(out)))
}
