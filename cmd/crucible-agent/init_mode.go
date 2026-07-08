//go:build linux

package main

import (
	"log/slog"
	"os"

	"golang.org/x/sys/unix"
)

// isInit reports whether the agent booted as PID 1 — i.e. the kernel
// exec'd it via init=/crucible/crucible-agent inside a converted OCI
// image, rather than systemd starting it inside a profile rootfs.
func isInit() bool {
	return os.Getpid() == 1
}

// mountSpec is one pseudo-filesystem the init agent mounts. A flattened
// container image has none of these — they are what the kernel and
// normal programs expect to exist.
type mountSpec struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
	// optional means a mount failure is logged but not fatal.
	optional bool
}

// pseudoMounts is the set the init agent establishes before serving,
// ordered so parents precede children (/dev before /dev/pts, /sys
// before the cgroup mount).
var pseudoMounts = []mountSpec{
	{source: "proc", target: "/proc", fstype: "proc", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
	{source: "sysfs", target: "/sys", fstype: "sysfs", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
	{source: "devtmpfs", target: "/dev", fstype: "devtmpfs", flags: unix.MS_NOSUID, data: "mode=0755"},
	{source: "devpts", target: "/dev/pts", fstype: "devpts", flags: unix.MS_NOSUID | unix.MS_NOEXEC, data: "gid=5,mode=0620"},
	{source: "tmpfs", target: "/dev/shm", fstype: "tmpfs", flags: unix.MS_NOSUID | unix.MS_NODEV, data: "mode=1777"},
	{source: "tmpfs", target: "/run", fstype: "tmpfs", flags: unix.MS_NOSUID | unix.MS_NODEV, data: "mode=0755"},
	{source: "cgroup2", target: "/sys/fs/cgroup", fstype: "cgroup2", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, optional: true},
}

// mountPseudoFilesystems establishes the standard mounts an init needs.
// Each target is created if absent. A failure on a required mount is
// returned (the guest can't function without /proc or /dev); optional
// mounts (cgroup2) only log. Idempotent: an already-mounted target
// yields EBUSY, which we treat as success.
func mountPseudoFilesystems(log *slog.Logger) error {
	for _, m := range pseudoMounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			if m.optional {
				log.Warn("init: create mount point failed", "target", m.target, "err", err)
				continue
			}
			return err
		}
		err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data)
		switch {
		case err == nil:
		case err == unix.EBUSY:
			// Already mounted — fine (idempotent re-run).
		case m.optional:
			log.Warn("init: optional mount failed", "target", m.target, "fstype", m.fstype, "err", err)
		default:
			return err
		}
	}
	return nil
}
