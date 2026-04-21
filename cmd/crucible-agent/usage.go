//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// ioPollInterval is how often the I/O collector re-reads
// /proc/<pid>/io. Short enough that we don't miss the end-of-life
// values for sub-second commands, long enough that a tight loop
// (e.g. `yes | head`) doesn't pay noticeable collector overhead.
const ioPollInterval = 50 * time.Millisecond

// procIOStats holds the two counters we care about from
// /proc/<pid>/io.
type procIOStats struct {
	ReadBytes  int64
	WriteBytes int64
}

// readProcIO parses /proc/<pid>/io. Returns os.ErrNotExist if the
// process has been reaped already (file vanishes with the process).
//
// Format of the file (one key: value pair per line):
//
//	rchar: 123
//	wchar: 456
//	syscr: 7
//	syscw: 8
//	read_bytes: 99
//	write_bytes: 100
//	cancelled_write_bytes: 0
//
// We only pull read_bytes + write_bytes because those are the ones
// the kernel attributes to actual storage I/O — the *char counters
// include read()/write() bytes satisfied by the page cache and
// aren't a useful "how much did I touch the disk?" signal.
func readProcIO(pid int) (procIOStats, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return procIOStats{}, err
	}
	defer f.Close()

	var stats procIOStats
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := line[:colon]
		val := strings.TrimSpace(line[colon+1:])
		switch key {
		case "read_bytes":
			stats.ReadBytes, _ = strconv.ParseInt(val, 10, 64)
		case "write_bytes":
			stats.WriteBytes, _ = strconv.ParseInt(val, 10, 64)
		}
	}
	return stats, scanner.Err()
}

// pollIO polls /proc/<pid>/io at ioPollInterval and stores the most
// recent successful read in last. The goroutine exits when stop is
// closed OR when readProcIO starts returning os.ErrNotExist (the
// child has been reaped and /proc/<pid> is gone). done is closed on
// exit to signal the caller it's safe to read last.
//
// Known limitation: for processes that exit and get reaped between
// two polls, the last stored value will be the most recent
// intermediate snapshot, not the true final values. This is usually
// fine for commands that run for more than a few hundred
// milliseconds; shorter commands may under-count. A precise
// solution needs per-exec cgroups (io.stat), which is a v0.2 item.
func pollIO(pid int, last *atomic.Pointer[procIOStats], stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(ioPollInterval)
	defer ticker.Stop()

	// Take one read immediately so we have a baseline even for
	// processes that finish before the first tick.
	if stats, err := readProcIO(pid); err == nil {
		last.Store(&stats)
	}

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			stats, err := readProcIO(pid)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return
				}
				// Transient read error (EAGAIN etc.); keep trying.
				continue
			}
			last.Store(&stats)
		}
	}
}

// buildUsage packages Rusage and optional I/O stats into the wire
// shape. Rusage is assumed non-nil (ProcessState.SysUsage returns
// non-nil on Linux whenever the process has been reaped). ioStats
// may be nil when the process finished before any poll succeeded;
// we treat that as "unknown, leave zero."
func buildUsage(ru *syscall.Rusage, ioStats *procIOStats) *agentwire.ResourceUsage {
	u := &agentwire.ResourceUsage{
		CPUUserMs:                  timevalToMs(ru.Utime),
		CPUSysMs:                   timevalToMs(ru.Stime),
		PeakMemoryBytes:            int64(ru.Maxrss) * 1024, // Linux reports Maxrss in KiB.
		PageFaultsMajor:            int64(ru.Majflt),
		ContextSwitchesInvoluntary: int64(ru.Nivcsw),
	}
	if ioStats != nil {
		u.IOReadBytes = ioStats.ReadBytes
		u.IOWriteBytes = ioStats.WriteBytes
	}
	return u
}

// timevalToMs converts a syscall.Timeval (seconds + microseconds
// since some epoch) into integer milliseconds.
func timevalToMs(tv syscall.Timeval) int64 {
	return int64(tv.Sec)*1000 + int64(tv.Usec)/1000
}

// detectOOM is the best-effort heuristic for "was this killed by
// OOM?"
//
// We set OomKilled when all of:
//
//  1. The process was killed by SIGKILL.
//  2. The kill wasn't ours (i.e. not from a client-requested
//     timeout — TimedOut is set separately).
//  3. Peak RSS reached at least 95% of the guest's total memory,
//     implying the VM was under memory pressure at the time.
//
// This catches genuine OOM kills (kernel OOM killer targeting the
// command) without false-positiving on SIGKILLs we issued ourselves.
// It misses OOMs where the process was killed while still under the
// 95% threshold (possible in cgroup-memory-constrained children
// even when the VM itself has headroom) — again, per-exec cgroups
// fix that in v0.2.
func detectOOM(ps *os.ProcessState, timedOut bool, peakRSSBytes int64, guestMemBytes int64) bool {
	if timedOut || ps == nil {
		return false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		return false
	}
	if guestMemBytes <= 0 || peakRSSBytes <= 0 {
		return false
	}
	// peakRSSBytes >= 0.95 * guestMemBytes, done with integers to
	// avoid float jitter around the threshold.
	return peakRSSBytes*100 >= guestMemBytes*95
}

// guestMemTotalBytes reads MemTotal from /proc/meminfo and returns
// it as bytes. Returns 0 on any error — that's a signal to
// detectOOM to not fire (rather than fire with a garbage threshold).
//
// /proc/meminfo's MemTotal is typically slightly less than the
// dmesg-reported memory (kernel reserves some); for the purposes
// of an OOM heuristic this is fine — we just want to know whether
// the process came close to whatever ceiling the guest saw.
func guestMemTotalBytes() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// Format: "MemTotal:       524288 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
