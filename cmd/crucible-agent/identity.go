//go:build linux

package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/gnana997/crucible/internal/agentwire"
)

// identitySeedSize is the exact seed length the handler accepts. The
// host sends 32 bytes (256 bits) — a full reseed's worth for the
// kernel CRNG; anything else is a protocol error, not something to
// work around.
const identitySeedSize = 32

// Linux random-device ioctls (linux/random.h). Stable kernel ABI.
const (
	// rndAddEntropy is _IOW('R', 0x03, int[2]): feed a
	// rand_pool_info — {entropy_count (bits), buf_size, buf[]} —
	// into the kernel input pool, crediting entropy_count bits.
	rndAddEntropy = 0x40085203

	// rndReseedCRNG is _IO('R', 0x07): reseed the CRNG from the
	// input pool now instead of on the kernel's own schedule.
	rndReseedCRNG = 0x5207
)

// Paths the identity refresh rewrites. Package-level vars so tests can
// redirect them into a temp dir.
var (
	randomDevicePath  = "/dev/urandom"
	machineIDPath     = "/etc/machine-id"
	dbusMachineIDPath = "/var/lib/dbus/machine-id"
	hostnamePath      = "/etc/hostname"
	forkIDPath        = "/run/crucible/fork-id"
)

// entropyInjector and hostnameSetter are seams for tests — the real
// implementations need root and mutate kernel state.
var (
	entropyInjector = injectEntropy
	hostnameSetter  = syscall.Sethostname
)

// injectEntropy credits seed to the kernel entropy pool
// (RNDADDENTROPY) and forces an immediate CRNG reseed
// (RNDRESEEDCRNG). Both ioctls need CAP_SYS_ADMIN; the agent runs as
// root. After this returns, getrandom()/urandom draws in this guest
// have diverged from every other fork of the same snapshot, even on
// kernels without CONFIG_VMGENID.
func injectEntropy(seed []byte) error {
	f, err := os.OpenFile(randomDevicePath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", randomDevicePath, err)
	}
	defer func() { _ = f.Close() }()

	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	info := make([]byte, 8+len(seed))
	binary.NativeEndian.PutUint32(info[0:4], uint32(len(seed)*8))
	binary.NativeEndian.PutUint32(info[4:8], uint32(len(seed)))
	copy(info[8:], seed)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&info[0]))); errno != 0 {
		return fmt.Errorf("ioctl RNDADDENTROPY: %w", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), rndReseedCRNG, 0); errno != 0 {
		return fmt.Errorf("ioctl RNDRESEEDCRNG: %w", errno)
	}
	return nil
}

// replaceFile atomically replaces path with content via a same-dir
// temp file and rename. Rename needs write permission on the
// directory, not the file, so this also replaces the 0444
// /etc/machine-id without relying on root's CAP_DAC_OVERRIDE — and no
// reader ever observes a truncated in-between state.
func replaceFile(path string, content []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// newMachineID returns a fresh machine-id: 16 random bytes as 32
// lowercase hex characters. Drawn via crypto/rand (getrandom), i.e.
// from the just-reseeded CRNG, so it is unique per fork.
func newMachineID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// handleIdentityRefresh gives a freshly-forked guest unique state.
// Called by sandbox.Manager.Fork right after the fork VM resumes and
// before the sandbox becomes execable (see docs/clone-safety.md).
// Steps, in order:
//
//  1. Credit the host-supplied 32-byte seed to the kernel entropy
//     pool and force a CRNG reseed. This is the portable half of the
//     clone-safety guarantee — it works on any guest kernel, and the
//     seed is unique per fork by construction. (VMGenID, when the
//     kernel supports it, reseeds even earlier — at resume — and
//     remains the primary line for processes already running; this
//     step removes the kernel-config dependency.)
//  2. Rewrite /etc/machine-id (and /var/lib/dbus/machine-id when it
//     is a regular file rather than the usual symlink) with a fresh
//     id drawn after the reseed.
//  3. Set the hostname — kernel and /etc/hostname — to the fork's
//     sandbox ID.
//  4. Write /run/crucible/fork-id, the app-level fork marker. Guest
//     code holding use-once state can watch this file to learn it
//     was cloned; rotating app-held secrets themselves is impossible
//     from outside the process.
//
// Every step is fatal on failure — unlike the network refresh, there
// is no self-healing fallback for duplicated entropy, and the caller
// treats a failed refresh as a failed fork.
func handleIdentityRefresh(w http.ResponseWriter, r *http.Request) {
	var req agentwire.IdentityRefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w,
			fmt.Sprintf("identity refresh failed (decode): %v", err),
			http.StatusBadRequest,
		)
		return
	}
	if len(req.Seed) != identitySeedSize {
		http.Error(w,
			fmt.Sprintf("identity refresh failed (seed): got %d bytes, want %d", len(req.Seed), identitySeedSize),
			http.StatusBadRequest,
		)
		return
	}
	if req.SandboxID == "" {
		http.Error(w,
			"identity refresh failed (sandbox_id): empty",
			http.StatusBadRequest,
		)
		return
	}

	if err := entropyInjector(req.Seed); err != nil {
		slog.Error("entropy injection failed", "err", err)
		http.Error(w,
			fmt.Sprintf("identity refresh failed (entropy): %v", err),
			http.StatusInternalServerError,
		)
		return
	}

	id, err := newMachineID()
	if err != nil {
		slog.Error("machine-id generation failed", "err", err)
		http.Error(w,
			fmt.Sprintf("identity refresh failed (machine-id): %v", err),
			http.StatusInternalServerError,
		)
		return
	}
	if err := replaceFile(machineIDPath, []byte(id+"\n"), 0o444); err != nil {
		slog.Error("machine-id rewrite failed", "path", machineIDPath, "err", err)
		http.Error(w,
			fmt.Sprintf("identity refresh failed (machine-id): %v", err),
			http.StatusInternalServerError,
		)
		return
	}
	// Ubuntu's /var/lib/dbus/machine-id is normally a symlink to
	// /etc/machine-id — already covered by the rewrite above. Rewrite
	// it separately only when an image variant made it a regular file.
	if fi, err := os.Lstat(dbusMachineIDPath); err == nil && fi.Mode().IsRegular() {
		if err := replaceFile(dbusMachineIDPath, []byte(id+"\n"), 0o444); err != nil {
			slog.Error("dbus machine-id rewrite failed", "path", dbusMachineIDPath, "err", err)
			http.Error(w,
				fmt.Sprintf("identity refresh failed (dbus machine-id): %v", err),
				http.StatusInternalServerError,
			)
			return
		}
	}

	if err := hostnameSetter([]byte(req.SandboxID)); err != nil {
		slog.Error("sethostname failed", "hostname", req.SandboxID, "err", err)
		http.Error(w,
			fmt.Sprintf("identity refresh failed (hostname): %v", err),
			http.StatusInternalServerError,
		)
		return
	}
	if err := os.WriteFile(hostnamePath, []byte(req.SandboxID+"\n"), 0o644); err != nil {
		slog.Error("hostname file rewrite failed", "path", hostnamePath, "err", err)
		http.Error(w,
			fmt.Sprintf("identity refresh failed (hostname file): %v", err),
			http.StatusInternalServerError,
		)
		return
	}

	if err := os.MkdirAll(filepath.Dir(forkIDPath), 0o755); err != nil {
		slog.Error("fork-id dir create failed", "path", forkIDPath, "err", err)
		http.Error(w,
			fmt.Sprintf("identity refresh failed (fork-id): %v", err),
			http.StatusInternalServerError,
		)
		return
	}
	if err := os.WriteFile(forkIDPath, []byte(req.SandboxID+"\n"), 0o644); err != nil {
		slog.Error("fork-id write failed", "path", forkIDPath, "err", err)
		http.Error(w,
			fmt.Sprintf("identity refresh failed (fork-id): %v", err),
			http.StatusInternalServerError,
		)
		return
	}

	slog.Info("identity refreshed", "sandbox_id", req.SandboxID, "machine_id", id)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
