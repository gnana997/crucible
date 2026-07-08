//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// servicePidFile records the supervised entrypoint's pid so a restarted
// agent (crashed and relaunched by systemd within the same boot — /run
// is tmpfs, so the file never survives a reboot) can find the orphan it
// lost. Policy is deliberately kill-and-report, not adopt: a non-child
// can't be waited on, so a fresh agent kills the orphaned group, logs
// loudly, and reports state idle for the host to re-configure.
const servicePidFile = "/run/crucible/service.pid"

// writeServicePidFile records "pid starttime" — starttime (clock ticks
// since boot, field 22 of /proc/<pid>/stat) guards the later kill
// against pid reuse.
func writeServicePidFile(path string, pid int) error {
	start, err := procStartTime(pid)
	if err != nil {
		return fmt.Errorf("read start time of pid %d: %w", pid, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d %d\n", pid, start)), 0o644)
}

func removeServicePidFile(path string) {
	_ = os.Remove(path)
}

// reconcileStaleService handles the agent-crash case at startup: if a
// previous agent left a pidfile and that exact process (pid + start
// time) is still alive, SIGKILL its process group and remove the file.
// Called before the supervisor accepts any commands.
func reconcileStaleService(path string, log *slog.Logger) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // no pidfile — the common case
	}
	defer removeServicePidFile(path)

	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		log.Warn("malformed service pidfile, removing", "path", path)
		return
	}
	pid, err1 := strconv.Atoi(fields[0])
	start, err2 := strconv.ParseUint(fields[1], 10, 64)
	if err1 != nil || err2 != nil || pid <= 0 {
		log.Warn("malformed service pidfile, removing", "path", path)
		return
	}

	liveStart, err := procStartTime(pid)
	if err != nil || liveStart != start {
		// Process is gone (or the pid was reused by something else):
		// nothing to kill.
		return
	}

	// The orphaned service from a previous agent run. Kill the whole
	// group; -pid works because launch() puts the entrypoint in its own
	// group with pgid == pid.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		log.Warn("kill orphaned service group failed", "pid", pid, "err", err)
		return
	}
	log.Warn("killed service orphaned by a previous agent run; host must re-configure",
		"pid", pid)
}

// procStartTime returns field 22 of /proc/<pid>/stat (starttime, clock
// ticks since boot) — the standard pid-reuse guard. The comm field
// (field 2) can contain spaces and parens, so parsing starts after the
// last ')'.
func procStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(data)
	close := strings.LastIndexByte(s, ')')
	if close < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	// After ") " the fields resume at field 3 (state); starttime is
	// field 22, i.e. index 19 here.
	fields := strings.Fields(s[close+1:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
