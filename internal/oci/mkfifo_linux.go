package oci

import "golang.org/x/sys/unix"

// mkfifo creates a named pipe. Split out for the build tag; the oci
// package's mkfs/fsck shell-outs are linux-only in practice, but this
// is the one direct syscall.
func mkfifo(path string, mode uint32) error {
	return unix.Mkfifo(path, mode)
}
