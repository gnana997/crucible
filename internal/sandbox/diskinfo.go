package sandbox

import (
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/sys/unix"
)

// ErrInsufficientDisk is returned by a sleep when free disk under WorkBase is
// below ManagerConfig.SleepMinFreeDiskMiB. The app stays running rather than
// writing a snapshot that could fill the disk; a later sleep can retry once
// space is freed.
var ErrInsufficientDisk = errors.New("sandbox: insufficient host disk to snapshot")

// admitSleep enforces the sleep-admission disk floor. A no-op when
// SleepMinFreeDiskMiB <= 0. A read error is logged and admits (fail-open) rather
// than wedging sleeps on a statfs hiccup — the floor is a safety net, not a hard
// gate, mirroring admitWake.
func (m *Manager) admitSleep() error {
	if m.cfg.SleepMinFreeDiskMiB <= 0 {
		return nil
	}
	read := m.cfg.DiskFreeMiB
	if read == nil {
		read = diskFreeMiB
	}
	free, err := read(m.cfg.WorkBase)
	if err != nil {
		slog.Default().Warn("sleep admission: read free disk failed; admitting",
			"component", "sandbox", "path", m.cfg.WorkBase, "err", err)
		return nil
	}
	if free < m.cfg.SleepMinFreeDiskMiB {
		return fmt.Errorf("%w: %d MiB free < %d MiB floor at %s", ErrInsufficientDisk, free, m.cfg.SleepMinFreeDiskMiB, m.cfg.WorkBase)
	}
	return nil
}

// diskFreeMiB returns the disk space available to an unprivileged writer under
// path, in MiB (statfs f_bavail × f_bsize). It is the default source for the
// sleep-admission check; tests inject a stub via ManagerConfig.DiskFreeMiB.
func diskFreeMiB(path string) (int, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// Bavail is blocks free for non-root; Bsize is the fragment size in bytes.
	free := int64(st.Bavail) * int64(st.Bsize)
	return int(free / (1 << 20)), nil
}
