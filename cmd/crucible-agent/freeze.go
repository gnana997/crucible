//go:build linux

package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gnana997/crucible/sdk/wire"
)

// freezeWatchdogTimeout auto-thaws a frozen filesystem if the paired /thaw never
// arrives (e.g. the daemon died mid-backup), so the guest can never stay wedged
// on a lost freeze. The host copy it brackets is O(1) (reflink) + a fsync, well
// under this bound.
const freezeWatchdogTimeout = 60 * time.Second

// FIFREEZE/FITHAW freeze/thaw a filesystem via a fd on its mountpoint
// (_IOWR('X', 119/120, int)). The numbers are the same on every architecture
// crucible targets (amd64/arm64 use asm-generic ioctl encoding); defined here
// because older golang.org/x/sys/unix does not export the constants.
const (
	fiFreeze = 0xc0045877
	fiThaw   = 0xc0045878
)

var (
	freezeMu     sync.Mutex
	freezeTimers = map[string]*time.Timer{} // mountpoint -> auto-thaw timer
)

// ioctlFreeze issues FIFREEZE or FITHAW on the filesystem at mountpoint via a
// short-lived directory fd. The freeze/thaw persists past the fd close (it acts
// on the superblock, not the fd), so no fd is retained between /freeze and /thaw.
func ioctlFreeze(mountpoint string, op uint) error {
	fd, err := unix.Open(mountpoint, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	// FIFREEZE/FITHAW ignore the arg; 0 is conventional.
	return unix.IoctlSetInt(fd, op, 0)
}

func cleanFreezeTarget(mp string) (string, bool) {
	clean := filepath.Clean(mp)
	return clean, filepath.IsAbs(clean) && clean != "/"
}

// handleFreeze FIFREEZEs the volume's filesystem so the host can copy its backing
// file consistently while the guest runs. Only the given mountpoint is frozen —
// never "/", so the agent (which lives on the rootfs) stays responsive to /thaw.
// Arms a watchdog that auto-thaws if /thaw is lost.
func handleFreeze(w http.ResponseWriter, r *http.Request) {
	var spec wire.FreezeSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, "invalid freeze spec: "+err.Error(), http.StatusBadRequest)
		return
	}
	clean, ok := cleanFreezeTarget(spec.Mountpoint)
	if !ok {
		http.Error(w, "mountpoint must be an absolute path below root", http.StatusBadRequest)
		return
	}
	if err := ioctlFreeze(clean, fiFreeze); err != nil {
		http.Error(w, "freeze "+clean+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	armThawWatchdog(clean)
	w.WriteHeader(http.StatusNoContent)
}

// handleThaw FITHAWs a previously frozen filesystem and cancels its watchdog.
func handleThaw(w http.ResponseWriter, r *http.Request) {
	var spec wire.FreezeSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, "invalid freeze spec: "+err.Error(), http.StatusBadRequest)
		return
	}
	clean, ok := cleanFreezeTarget(spec.Mountpoint)
	if !ok {
		http.Error(w, "mountpoint must be an absolute path below root", http.StatusBadRequest)
		return
	}
	cancelThawWatchdog(clean)
	if err := ioctlFreeze(clean, fiThaw); err != nil {
		http.Error(w, "thaw "+clean+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func armThawWatchdog(mp string) {
	freezeMu.Lock()
	defer freezeMu.Unlock()
	if t, ok := freezeTimers[mp]; ok {
		t.Stop()
	}
	freezeTimers[mp] = time.AfterFunc(freezeWatchdogTimeout, func() {
		freezeMu.Lock()
		delete(freezeTimers, mp)
		freezeMu.Unlock()
		_ = ioctlFreeze(mp, fiThaw) // best-effort recovery
	})
}

func cancelThawWatchdog(mp string) {
	freezeMu.Lock()
	defer freezeMu.Unlock()
	if t, ok := freezeTimers[mp]; ok {
		t.Stop()
		delete(freezeTimers, mp)
	}
}
