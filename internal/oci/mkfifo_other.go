//go:build !linux

package oci

import "errors"

// mkfifo is unsupported off Linux; the oci pipeline only runs on the
// Linux daemon, but this keeps the package compiling for tooling.
func mkfifo(path string, mode uint32) error {
	return errors.New("oci: mkfifo unsupported on this platform")
}
