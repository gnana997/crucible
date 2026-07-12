package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// ErrInsufficientMemory is returned by a wake when the host has too little free
// memory to admit it (below ManagerConfig.WakeMinFreeMiB). The proxy surfaces it
// as a clean 503; the app stays asleep and a later wake can retry.
var ErrInsufficientMemory = errors.New("sandbox: insufficient host memory to wake")

// admitWake enforces the wake-admission floor. A no-op when WakeMinFreeMiB <= 0.
// A read error is logged and admits (fail-open) rather than wedging wakes on a
// meminfo hiccup — the floor is a safety net, not a hard gate.
func (m *Manager) admitWake() error {
	if m.cfg.WakeMinFreeMiB <= 0 {
		return nil
	}
	read := m.cfg.MemAvailableMiB
	if read == nil {
		read = memAvailableMiB
	}
	avail, err := read()
	if err != nil {
		slog.Default().Warn("wake admission: read available memory failed; admitting",
			"component", "sandbox", "err", err)
		return nil
	}
	if avail < m.cfg.WakeMinFreeMiB {
		return fmt.Errorf("%w: %d MiB available < %d MiB floor", ErrInsufficientMemory, avail, m.cfg.WakeMinFreeMiB)
	}
	return nil
}

// memAvailableMiB reads MemAvailable from /proc/meminfo (the kernel's estimate
// of memory available for starting new workloads without swapping) and returns
// it in MiB. It is the default source for the wake-admission check; tests inject
// a stub via ManagerConfig.MemAvailableMiB.
func memAvailableMiB() (int, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		// Format: "MemAvailable:   12345678 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("meminfo: malformed MemAvailable line %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("meminfo: parse MemAvailable: %w", err)
		}
		return int(kb / 1024), nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("meminfo: MemAvailable not found")
}
