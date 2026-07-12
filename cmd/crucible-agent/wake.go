//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"syscall"

	"github.com/gnana997/crucible/internal/agentwire"
)

// maxWakeRequestBody bounds the POST /wake body — a small JSON blob (a base64
// seed + an int64), so cap it like the identity handler does.
const maxWakeRequestBody = 1 << 20

// clockStepper is a seam for tests; the real implementation needs CAP_SYS_TIME.
var clockStepper = stepClock

// stepClock sets the guest's CLOCK_REALTIME to the given Unix-nanosecond wall
// time. settimeofday(2) needs CAP_SYS_TIME; the agent runs as root. Microsecond
// resolution is ample for correcting a sleep gap measured in seconds-to-days.
func stepClock(unixNano int64) error {
	tv := syscall.NsecToTimeval(unixNano)
	if err := syscall.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}
	return nil
}

// handleWake restores a slept guest's non-persistent state after a
// wake-in-place restore, WITHOUT rotating identity (that is the wake-vs-fork
// distinction). In order:
//
//  1. Credit the host-supplied 32-byte seed to the kernel entropy pool and
//     force a CRNG reseed — every wake of one snapshot otherwise replays
//     identical entropy.
//  2. Step CLOCK_REALTIME to the host's wall time — a stale clock silently
//     breaks TLS, token expiry, and cache TTLs.
//
// Both steps are fatal on failure: the host runs this before making the woken
// guest reachable, and neither hazard self-heals.
func handleWake(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWakeRequestBody)
	var req agentwire.WakeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("wake failed (decode): %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Seed) != identitySeedSize {
		http.Error(w,
			fmt.Sprintf("wake failed (seed): got %d bytes, want %d", len(req.Seed), identitySeedSize),
			http.StatusBadRequest)
		return
	}
	if req.WallTimeUnixNano <= 0 {
		http.Error(w, "wake failed (wall_time_unix_nano): required, must be > 0", http.StatusBadRequest)
		return
	}

	if err := entropyInjector(req.Seed); err != nil {
		slog.Error("wake: entropy injection failed", "err", err)
		http.Error(w, fmt.Sprintf("wake failed (entropy): %v", err), http.StatusInternalServerError)
		return
	}
	if err := clockStepper(req.WallTimeUnixNano); err != nil {
		slog.Error("wake: clock step failed", "err", err)
		http.Error(w, fmt.Sprintf("wake failed (clock): %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("woke", "wall_time_unix_nano", req.WallTimeUnixNano)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
