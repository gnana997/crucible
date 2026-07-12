package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// captureEnv gathers the host + storage facts that make a benchmark comparable
// and honest. The reflink probe is load-bearing for crucible: snapshot / fork /
// wake reflink the memory + rootfs files on XFS/btrfs (near-instant) but fall
// back to a full byte-copy on ext4 — so the same numbers on ext4 vs btrfs differ
// by an order of magnitude. Any published wake latency MUST report this.
func captureEnv(reflinkPath string) map[string]any {
	host, _ := os.Hostname()
	kernel := "?"
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		kernel = strings.TrimSpace(string(b))
	}
	if reflinkPath == "" {
		reflinkPath = os.TempDir()
	}
	ok, note := reflinkSupported(reflinkPath)
	return map[string]any{
		"host":              host,
		"os":                runtime.GOOS,
		"arch":              runtime.GOARCH,
		"num_cpu":           runtime.NumCPU(),
		"cpu_model":         firstMatch("/proc/cpuinfo", "model name"),
		"kernel":            kernel,
		"mem_total_mib":     parseKBLineMiB("/proc/meminfo", "MemTotal:"),
		"mem_available_mib": memAvailableMiB(),
		"reflink": map[string]any{
			"path":      reflinkPath,
			"supported": ok,
			"note":      note,
		},
	}
}

// reflinkSupported attempts a real FICLONE in dir and reports whether it worked.
// false means crucible's snapshot/fork/wake byte-copy on this storage (slow).
func reflinkSupported(dir string) (bool, string) {
	src, err := os.CreateTemp(dir, "crucible-bench-reflink-")
	if err != nil {
		return false, "probe create: " + err.Error()
	}
	srcName := src.Name()
	defer func() { _ = os.Remove(srcName) }()
	if _, err := src.WriteString("crucible reflink probe"); err != nil {
		_ = src.Close()
		return false, "probe write: " + err.Error()
	}
	dstName := srcName + "-dst"
	dst, err := os.Create(dstName)
	if err != nil {
		_ = src.Close()
		return false, "probe create dst: " + err.Error()
	}
	defer func() { _ = os.Remove(dstName) }()

	err = unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
	_ = dst.Close()
	_ = src.Close()
	if err != nil {
		return false, fmt.Sprintf("FICLONE failed (%v) — filesystem does not support reflink; snapshot/fork/wake will byte-copy", err)
	}
	return true, "FICLONE ok — snapshot/fork/wake reflink (fast)"
}

// parseKBLineMiB reads a "Key:  <n> kB" line from a /proc file and returns MiB.
func parseKBLineMiB(path, prefix string) int64 {
	v := firstMatch(path, prefix) // e.g. "16302344 kB"
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0
	}
	kb, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return kb / 1024
}
