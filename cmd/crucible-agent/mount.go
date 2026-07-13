//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"

	"github.com/gnana997/crucible/sdk/wire"
)

// volDeviceRe bounds which block devices /mount will touch: a secondary
// virtio-blk device (/dev/vdb…/dev/vdz). Never /dev/vda (the rootfs).
var volDeviceRe = regexp.MustCompile(`^/dev/vd[b-z]$`)

// handleMount mounts a persistent volume's block device at a path inside the
// guest. The daemon calls it over vsock after /healthz and before the
// workload starts, so a volume-backed app sees its data directory in place.
// Idempotent: an already-mounted target (EBUSY) is treated as success, so a
// wake-path re-mount is safe. Reuses the init-mode mount contract
// (mkdir target, unix.Mount, EBUSY == ok — see init_mode.go).
func handleMount(w http.ResponseWriter, r *http.Request) {
	var m wire.MountSpec
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "invalid mount spec: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !volDeviceRe.MatchString(m.Device) {
		http.Error(w, "invalid device (want /dev/vd[b-z]): "+m.Device, http.StatusBadRequest)
		return
	}
	clean := filepath.Clean(m.Mountpoint)
	if !filepath.IsAbs(clean) || clean == "/" {
		http.Error(w, "mountpoint must be an absolute path below root", http.StatusBadRequest)
		return
	}
	fstype := m.Fstype
	if fstype == "" {
		fstype = "ext4"
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		http.Error(w, "create mountpoint: "+err.Error(), http.StatusInternalServerError)
		return
	}
	err := unix.Mount(m.Device, clean, fstype, 0, "")
	switch {
	case err == nil, errors.Is(err, unix.EBUSY):
		// mounted, or already mounted there — success.
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "mount "+m.Device+" at "+clean+": "+err.Error(), http.StatusInternalServerError)
	}
}
