//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
)

// withIdentityPaths redirects every file the handler rewrites into a
// temp dir for the duration of a test. forkIDPath keeps its run/crucible
// parent so the handler's MkdirAll is exercised too.
func withIdentityPaths(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origMachine, origDbus := machineIDPath, dbusMachineIDPath
	origHostname, origForkID := hostnamePath, forkIDPath
	machineIDPath = filepath.Join(dir, "machine-id")
	dbusMachineIDPath = filepath.Join(dir, "dbus-machine-id")
	hostnamePath = filepath.Join(dir, "hostname")
	forkIDPath = filepath.Join(dir, "run", "crucible", "fork-id")
	t.Cleanup(func() {
		machineIDPath, dbusMachineIDPath = origMachine, origDbus
		hostnamePath, forkIDPath = origHostname, origForkID
	})
	return dir
}

// withEntropyInjector swaps the RNDADDENTROPY/RNDRESEEDCRNG seam for a
// stub, recording the seeds it was handed.
func withEntropyInjector(t *testing.T, hook func(seed []byte) error) *[][]byte {
	t.Helper()
	var seeds [][]byte
	orig := entropyInjector
	entropyInjector = func(seed []byte) error {
		seeds = append(seeds, append([]byte(nil), seed...))
		return hook(seed)
	}
	t.Cleanup(func() { entropyInjector = orig })
	return &seeds
}

// withHostnameSetter swaps the sethostname(2) seam for a stub.
func withHostnameSetter(t *testing.T, hook func(name []byte) error) *[]string {
	t.Helper()
	var names []string
	orig := hostnameSetter
	hostnameSetter = func(name []byte) error {
		names = append(names, string(name))
		return hook(name)
	}
	t.Cleanup(func() { hostnameSetter = orig })
	return &names
}

func identityBody(t *testing.T, seed []byte, sandboxID string) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(agentwire.IdentityRefreshRequest{Seed: seed, SandboxID: sandboxID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}

var machineIDRe = regexp.MustCompile(`^[0-9a-f]{32}\n$`)

func TestIdentityRefreshHappy(t *testing.T) {
	withIdentityPaths(t)
	// Pre-existing state from the snapshot: a stale machine-id and a
	// dbus copy that is a regular file (the image variant that needs
	// the separate rewrite).
	oldID := "00000000000000000000000000000000\n"
	if err := os.WriteFile(machineIDPath, []byte(oldID), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbusMachineIDPath, []byte(oldID), 0o444); err != nil {
		t.Fatal(err)
	}
	seed := bytes.Repeat([]byte{0x42}, identitySeedSize)
	seeds := withEntropyInjector(t, func([]byte) error { return nil })
	names := withHostnameSetter(t, func([]byte) error { return nil })

	req := httptest.NewRequest("POST", "/identity/refresh", identityBody(t, seed, "sb-fork-1"))
	w := httptest.NewRecorder()
	handleIdentityRefresh(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if len(*seeds) != 1 || !bytes.Equal((*seeds)[0], seed) {
		t.Errorf("entropy injector got %v, want exactly the request seed", *seeds)
	}

	gotID, err := os.ReadFile(machineIDPath)
	if err != nil {
		t.Fatal(err)
	}
	if !machineIDRe.Match(gotID) {
		t.Errorf("machine-id = %q, want 32 lowercase hex + newline", gotID)
	}
	if string(gotID) == oldID {
		t.Error("machine-id was not rotated")
	}
	gotDbus, err := os.ReadFile(dbusMachineIDPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotDbus, gotID) {
		t.Errorf("dbus machine-id = %q, want same as machine-id %q", gotDbus, gotID)
	}

	if len(*names) != 1 || (*names)[0] != "sb-fork-1" {
		t.Errorf("sethostname got %v, want [sb-fork-1]", *names)
	}
	gotHostname, err := os.ReadFile(hostnamePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotHostname) != "sb-fork-1\n" {
		t.Errorf("hostname file = %q, want sb-fork-1\\n", gotHostname)
	}

	gotForkID, err := os.ReadFile(forkIDPath)
	if err != nil {
		t.Fatalf("fork-id marker (MkdirAll path): %v", err)
	}
	if string(gotForkID) != "sb-fork-1\n" {
		t.Errorf("fork-id = %q, want sb-fork-1\\n", gotForkID)
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("body missing ok: %s", w.Body.String())
	}
}

func TestIdentityRefreshDbusSymlinkLeftAlone(t *testing.T) {
	withIdentityPaths(t)
	if err := os.WriteFile(machineIDPath, []byte("00000000000000000000000000000000\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// The normal Ubuntu layout: dbus machine-id is a symlink to
	// /etc/machine-id. The handler must not replace the link.
	if err := os.Symlink(machineIDPath, dbusMachineIDPath); err != nil {
		t.Fatal(err)
	}
	withEntropyInjector(t, func([]byte) error { return nil })
	withHostnameSetter(t, func([]byte) error { return nil })

	req := httptest.NewRequest("POST", "/identity/refresh",
		identityBody(t, make([]byte, identitySeedSize), "sb-fork-1"))
	w := httptest.NewRecorder()
	handleIdentityRefresh(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	fi, err := os.Lstat(dbusMachineIDPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("dbus machine-id symlink was replaced by a regular file")
	}
	// Reading through the symlink must yield the rotated id.
	viaLink, err := os.ReadFile(dbusMachineIDPath)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := os.ReadFile(machineIDPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaLink, direct) {
		t.Errorf("dbus id via symlink = %q, machine-id = %q; want identical", viaLink, direct)
	}
}

func TestIdentityRefreshUniquePerCall(t *testing.T) {
	withIdentityPaths(t)
	withEntropyInjector(t, func([]byte) error { return nil })
	withHostnameSetter(t, func([]byte) error { return nil })

	ids := make(map[string]bool)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/identity/refresh",
			identityBody(t, make([]byte, identitySeedSize), "sb-fork-1"))
		w := httptest.NewRecorder()
		handleIdentityRefresh(w, req)
		if w.Code != 200 {
			t.Fatalf("call %d: status = %d: %s", i, w.Code, w.Body.String())
		}
		id, err := os.ReadFile(machineIDPath)
		if err != nil {
			t.Fatal(err)
		}
		ids[string(id)] = true
	}
	if len(ids) != 2 {
		t.Errorf("machine-id repeated across refreshes: %v", ids)
	}
}

func TestIdentityRefreshRejectsBadJSON(t *testing.T) {
	withIdentityPaths(t)
	seeds := withEntropyInjector(t, func([]byte) error { return nil })

	req := httptest.NewRequest("POST", "/identity/refresh", strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	handleIdentityRefresh(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "identity refresh failed (decode)") {
		t.Errorf("body should name the decode step: %s", w.Body.String())
	}
	if len(*seeds) != 0 {
		t.Error("entropy must not be injected on a malformed request")
	}
}

func TestIdentityRefreshRejectsWrongSeedSize(t *testing.T) {
	withIdentityPaths(t)
	seeds := withEntropyInjector(t, func([]byte) error { return nil })

	req := httptest.NewRequest("POST", "/identity/refresh",
		identityBody(t, make([]byte, 16), "sb-fork-1"))
	w := httptest.NewRecorder()
	handleIdentityRefresh(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "identity refresh failed (seed)") {
		t.Errorf("body should name the seed step: %s", w.Body.String())
	}
	if len(*seeds) != 0 {
		t.Error("a short seed must not reach the kernel")
	}
}

func TestIdentityRefreshRejectsEmptySandboxID(t *testing.T) {
	withIdentityPaths(t)
	seeds := withEntropyInjector(t, func([]byte) error { return nil })

	req := httptest.NewRequest("POST", "/identity/refresh",
		identityBody(t, make([]byte, identitySeedSize), ""))
	w := httptest.NewRecorder()
	handleIdentityRefresh(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "identity refresh failed (sandbox_id)") {
		t.Errorf("body should name the sandbox_id step: %s", w.Body.String())
	}
	if len(*seeds) != 0 {
		t.Error("entropy must not be injected without a sandbox id")
	}
}

func TestIdentityRefreshEntropyFailureIs500(t *testing.T) {
	withIdentityPaths(t)
	oldID := "00000000000000000000000000000000\n"
	if err := os.WriteFile(machineIDPath, []byte(oldID), 0o444); err != nil {
		t.Fatal(err)
	}
	withEntropyInjector(t, func([]byte) error {
		return os.ErrPermission
	})
	withHostnameSetter(t, func([]byte) error {
		t.Error("hostname must not be touched when entropy injection fails")
		return nil
	})

	req := httptest.NewRequest("POST", "/identity/refresh",
		identityBody(t, make([]byte, identitySeedSize), "sb-fork-1"))
	w := httptest.NewRecorder()
	handleIdentityRefresh(w, req)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "identity refresh failed (entropy)") {
		t.Errorf("body should name the entropy step: %s", w.Body.String())
	}
	// Ordering guarantee: identifiers rotate only after the kernel
	// has fresh entropy — a failed injection leaves them untouched.
	got, err := os.ReadFile(machineIDPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != oldID {
		t.Error("machine-id must not be rotated when entropy injection fails")
	}
}

func TestIdentityRefreshHostnameFailureIs500(t *testing.T) {
	withIdentityPaths(t)
	withEntropyInjector(t, func([]byte) error { return nil })
	withHostnameSetter(t, func([]byte) error { return os.ErrPermission })

	req := httptest.NewRequest("POST", "/identity/refresh",
		identityBody(t, make([]byte, identitySeedSize), "sb-fork-1"))
	w := httptest.NewRecorder()
	handleIdentityRefresh(w, req)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "identity refresh failed (hostname)") {
		t.Errorf("body should name the hostname step: %s", w.Body.String())
	}
	if _, err := os.Stat(forkIDPath); !os.IsNotExist(err) {
		t.Error("fork-id marker must not be written when an earlier step fails")
	}
}
