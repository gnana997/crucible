//go:build linux

package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

func TestBuildServiceEnvProfileMerges(t *testing.T) {
	t.Setenv("CRUCIBLE_TEST_AGENTENV", "present")
	spec := &agentwire.ServiceSpec{Env: map[string]string{"APP": "1"}} // EnvExact false
	env := buildServiceEnv(spec, "")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "APP=1") {
		t.Errorf("spec env missing: %v", env)
	}
	if !strings.Contains(joined, "CRUCIBLE_TEST_AGENTENV=present") {
		t.Errorf("profile mode should merge agent environ: %v", env)
	}
}

func TestBuildServiceEnvExactNoAgentLeak(t *testing.T) {
	t.Setenv("CRUCIBLE_TEST_AGENTENV", "present")
	spec := &agentwire.ServiceSpec{Env: map[string]string{"APP": "1"}, EnvExact: true}
	env := buildServiceEnv(spec, "/home/nginx")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "CRUCIBLE_TEST_AGENTENV") {
		t.Errorf("exact env leaked agent environ: %v", env)
	}
	if !strings.Contains(joined, "APP=1") {
		t.Errorf("spec env missing: %v", env)
	}
	if !strings.Contains(joined, "PATH="+dockerDefaultPath) {
		t.Errorf("default PATH not added: %v", env)
	}
	if !strings.Contains(joined, "HOME=/home/nginx") {
		t.Errorf("HOME from user not added: %v", env)
	}
}

func TestBuildServiceEnvExactKeepsExplicitPathAndHome(t *testing.T) {
	spec := &agentwire.ServiceSpec{
		Env:      map[string]string{"PATH": "/only/here", "HOME": "/explicit"},
		EnvExact: true,
	}
	env := buildServiceEnv(spec, "/from/user")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=/only/here") || strings.Contains(joined, dockerDefaultPath) {
		t.Errorf("explicit PATH overwritten: %v", env)
	}
	if !strings.Contains(joined, "HOME=/explicit") || strings.Contains(joined, "/from/user") {
		t.Errorf("explicit HOME overwritten: %v", env)
	}
}

func TestResolveUserRoot(t *testing.T) {
	cred, home, err := resolveUser("0")
	if err != nil {
		t.Fatalf("resolveUser(0): %v", err)
	}
	if cred.Uid != 0 {
		t.Errorf("uid = %d, want 0", cred.Uid)
	}
	_ = home
}

func TestResolveUserCurrentByName(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}
	cred, home, err := resolveUser(u.Username)
	if err != nil {
		t.Fatalf("resolveUser(%q): %v", u.Username, err)
	}
	if strconv.FormatUint(uint64(cred.Uid), 10) != u.Uid {
		t.Errorf("uid = %d, want %s", cred.Uid, u.Uid)
	}
	if home != u.HomeDir {
		t.Errorf("home = %q, want %q", home, u.HomeDir)
	}
}

func TestResolveUserNumericUidGid(t *testing.T) {
	cred, _, err := resolveUser("1234:5678")
	if err != nil {
		t.Fatalf("resolveUser(1234:5678): %v", err)
	}
	if cred.Uid != 1234 || cred.Gid != 5678 {
		t.Errorf("uid/gid = %d/%d, want 1234/5678", cred.Uid, cred.Gid)
	}
	// Explicit group → no supplementary groups.
	if len(cred.Groups) != 0 {
		t.Errorf("explicit group carried supplementary groups: %v", cred.Groups)
	}
}

func TestResolveUserUnknownNameErrors(t *testing.T) {
	if _, _, err := resolveUser("definitely-no-such-user-xyz"); err == nil {
		t.Error("resolveUser of an unknown name succeeded")
	}
}

func TestResolveLaunchCreatesWorkdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "made", "by", "launch")
	spec := &agentwire.ServiceSpec{
		Cmd:      []string{"/bin/true"},
		Cwd:      dir,
		EnvExact: true,
	}
	if _, _, _, err := resolveLaunch(spec); err != nil {
		t.Fatalf("resolveLaunch: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("working dir not created: %v", err)
	}
}

func TestResolveLaunchProfileDoesNotCreateWorkdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "should-not-exist")
	spec := &agentwire.ServiceSpec{Cmd: []string{"/bin/true"}, Cwd: dir} // EnvExact false
	if _, _, _, err := resolveLaunch(spec); err != nil {
		t.Fatalf("resolveLaunch: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("profile mode created the working dir: %v", err)
	}
}

func TestLookExecutable(t *testing.T) {
	// A bare name resolves against PATH.
	dir := t.TempDir()
	bin := filepath.Join(dir, "myserver")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := lookExecutable("myserver", dir+":/nowhere", "")
	if err != nil || got != bin {
		t.Errorf("lookExecutable(bare) = %q, %v; want %q", got, err, bin)
	}

	// A non-executable file is skipped.
	nonexec := filepath.Join(dir, "data")
	_ = os.WriteFile(nonexec, []byte("x"), 0o644)
	if _, err := lookExecutable("data", dir, ""); err == nil {
		t.Error("lookExecutable resolved a non-executable file")
	}

	// Absolute paths pass through untouched.
	if got, _ := lookExecutable("/usr/local/bin/caddy", "/ignored", ""); got != "/usr/local/bin/caddy" {
		t.Errorf("absolute path = %q, want unchanged", got)
	}

	// Relative-with-slash resolves against cwd.
	if got, _ := lookExecutable("./run.sh", "", "/app"); got != "/app/run.sh" {
		t.Errorf("relative path = %q, want /app/run.sh", got)
	}

	// A bare name not on PATH errors.
	if _, err := lookExecutable("definitely-not-here", "/nowhere", ""); err == nil {
		t.Error("missing bare name did not error")
	}

	// Empty PATH falls back to the Docker default (finds /bin/sh).
	if got, err := lookExecutable("sh", "", ""); err != nil || got == "" {
		t.Errorf("empty PATH fallback failed: %q %v", got, err)
	}
}

func TestResolveLaunchResolvesBareCommandOnPath(t *testing.T) {
	// A bare-command OCI entrypoint (the httpd/redis/memcached case)
	// must resolve to an absolute path via the image's PATH.
	dir := t.TempDir()
	server := filepath.Join(dir, "httpd-foreground")
	if err := os.WriteFile(server, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := &agentwire.ServiceSpec{
		Cmd:      []string{"httpd-foreground"},
		Env:      map[string]string{"PATH": dir},
		EnvExact: true,
	}
	argv, _, _, err := resolveLaunch(spec)
	if err != nil {
		t.Fatalf("resolveLaunch: %v", err)
	}
	if argv[0] != server {
		t.Errorf("argv[0] = %q, want the resolved absolute path %q", argv[0], server)
	}
}

// TestInitRunnerDropsToUser verifies the credential actually takes
// effect: a service running as a non-root user reports that uid. Needs
// root (only root can setuid to another user).
func TestInitRunnerDropsToUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping to another user needs root")
	}
	r := newTestReaper(t)
	s := newSupervisor(initRunner{reaper: r}, realClock{}, testLogger(), "")
	t.Cleanup(func() { _, _ = s.Shutdown() })

	out := filepath.Join(t.TempDir(), "uid")
	// Run as uid 1000; write our effective uid then idle.
	spec := &agentwire.ServiceSpec{
		Cmd:      []string{"/bin/sh", "-c", "id -u > " + out + "; sleep 60"},
		User:     "1000:1000",
		EnvExact: true,
	}
	// The temp dir must be writable by uid 1000.
	_ = os.Chmod(filepath.Dir(out), 0o777)
	mustConfigureStart(t, s, spec)

	waitForFile(t, out)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read uid file: %v", err)
	}
	if strings.TrimSpace(string(data)) != "1000" {
		t.Errorf("service ran as uid %q, want 1000", strings.TrimSpace(string(data)))
	}
	_, _ = s.Stop(0)
	waitForState(t, s, agentwire.ServiceStateStopped)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s never written", path)
}
