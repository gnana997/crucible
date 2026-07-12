//go:build linux

package main

import (
	"encoding/json"
	"net/http"
	"syscall"
)

// syncer is a seam for tests; the real implementation is sync(2), which flushes
// every mounted filesystem's dirty page-cache back to its block device.
var syncer = syscall.Sync

// handleQuiesce flushes the guest's filesystems so a subsequent host-side
// snapshot/FICLONE of the rootfs is filesystem-clean rather than merely
// crash-consistent (used before sleep). sync(2) takes no
// arguments, cannot fail, and returns nothing, so this always succeeds — a
// caller that gets a non-2xx is talking to something other than this handler.
func handleQuiesce(w http.ResponseWriter, _ *http.Request) {
	syncer()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
