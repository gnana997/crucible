package jailer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

// seedOrphan creates a minimal chroot layout mimicking one a previous
// daemon run would have left behind. Used by reap tests.
func seedOrphan(t *testing.T, base, execBasename, id string) {
	t.Helper()
	chroot := filepath.Join(base, execBasename, id, "root")
	if err := os.MkdirAll(chroot, 0o750); err != nil {
		t.Fatalf("seed chroot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chroot, "stale.txt"), []byte("x"), 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}
}

func TestReapOrphansRemovesStaleChroots(t *testing.T) {
	base := t.TempDir()
	execFile := "/usr/bin/firecracker"

	for _, id := range []string{"sbx-a", "sbx-b", "sbx-c"} {
		seedOrphan(t, base, "firecracker", id)
	}

	reaped, err := ReapOrphans(base, execFile)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	slices.Sort(reaped)
	want := []string{"sbx-a", "sbx-b", "sbx-c"}
	if !slices.Equal(reaped, want) {
		t.Fatalf("reaped = %v, want %v", reaped, want)
	}

	// The parent dir should still exist (we only removed the children)
	// but it should be empty.
	parent := filepath.Join(base, "firecracker")
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("parent not empty: %v", entries)
	}
}

func TestKillJailedNoMatchIsNoop(t *testing.T) {
	// No process carries this (unique, nonexistent) jailer ID, so
	// killJailed must report the set as drained (nothing to kill) — the
	// safety property the graceful teardown path and the reap of
	// already-dead chroots both rely on.
	id := "sbx-no-such-vm-zzzq9x7k3rf22"
	if !killJailed(id) {
		t.Fatal("killJailed with no matching id returned false, want true (nothing to drain)")
	}
	if pids := jailedPIDs(id); len(pids) != 0 {
		t.Fatalf("jailedPIDs with no matching id = %v, want none", pids)
	}
}

func TestReapOrphansSkipsInvalidDirName(t *testing.T) {
	// A directory name that isn't a valid jailer ID is not one crucible
	// created, so the reap must never feed it to killJailed. ReapOrphans
	// must skip it, leave its tree in place, and surface an error —
	// while still reaping the valid sibling. ("bad_name" has an
	// underscore, which validIDPattern rejects.)
	base := t.TempDir()
	seedOrphan(t, base, "firecracker", "bad_name")
	seedOrphan(t, base, "firecracker", "sbx-real") // legitimate

	reaped, err := ReapOrphans(base, "/usr/bin/firecracker")
	if err == nil {
		t.Fatal("expected an error for the invalid dir name, got nil")
	}
	if len(reaped) != 1 || reaped[0] != "sbx-real" {
		t.Fatalf("reaped = %v, want [sbx-real]", reaped)
	}
	// The invalid dir's tree must be left untouched, not removed.
	if _, statErr := os.Stat(filepath.Join(base, "firecracker", "bad_name")); statErr != nil {
		t.Fatalf("invalid-name dir should be left in place, stat err = %v", statErr)
	}
}

func TestCmdlineMatchesIDRequiresDashIDAdjacency(t *testing.T) {
	nul := func(toks ...string) []byte {
		var b []byte
		for _, tk := range toks {
			b = append(b, []byte(tk)...)
			b = append(b, 0)
		}
		return b
	}
	cases := []struct {
		name string
		raw  []byte
		id   string
		want bool
	}{
		{"jailer argv", nul("/usr/bin/jailer", "--id", "sbx-abc", "--exec-file", "/fc"), "sbx-abc", true},
		{"firecracker argv", nul("firecracker", "--id", "sbx-abc"), "sbx-abc", true},
		// The M6 danger: ambiguous id appearing as a bare arg, NOT after --id.
		{"bare ambiguous token", nul("sleep", "1"), "1", false},
		{"id present but not after --id", nul("--exec-file", "sbx-abc", "--id", "other"), "sbx-abc", false},
		{"no match", nul("bash", "-c", "true"), "sbx-abc", false},
		{"empty cmdline", nil, "sbx-abc", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cmdlineMatchesID(c.raw, c.id); got != c.want {
				t.Errorf("cmdlineMatchesID(%q, %q) = %v, want %v", c.raw, c.id, got, c.want)
			}
		})
	}
}

// TestCmdlineArgExtraction covers the flag-value reader that scopes the
// live-orphan sweep: it must return the token after a flag, distinguish a
// present-but-empty value, and report absent flags.
func TestCmdlineArgExtraction(t *testing.T) {
	nul := func(toks ...string) []byte {
		var b []byte
		for _, tk := range toks {
			b = append(b, []byte(tk)...)
			b = append(b, 0)
		}
		return b
	}
	raw := nul("/usr/bin/jailer", "--id", "sbx-abc", "--exec-file", "/fc",
		"--chroot-base-dir", "/srv/jailer", "--cgroup-version", "2")

	if v, ok := cmdlineArg(raw, "--id"); !ok || v != "sbx-abc" {
		t.Errorf("--id = (%q, %v), want (sbx-abc, true)", v, ok)
	}
	if v, ok := cmdlineArg(raw, "--chroot-base-dir"); !ok || v != "/srv/jailer" {
		t.Errorf("--chroot-base-dir = (%q, %v), want (/srv/jailer, true)", v, ok)
	}
	if _, ok := cmdlineArg(raw, "--netns"); ok {
		t.Error("--netns should be absent")
	}
	// A trailing flag with no following token is treated as absent (there is
	// no value after it), which is the safe read.
	if _, ok := cmdlineArg(nul("jailer", "--id"), "--id"); ok {
		t.Error("--id with no following token should read as absent")
	}
}

// TestKillLiveOrphansScopedByChrootBase drives the live-orphan sweep against
// a real process whose argv carries jailer's scoping tokens: the sweep must
// find and kill it, must be scoped to its chroot-base (a different base
// finds nothing), and must leave nothing behind.
func TestKillLiveOrphansScopedByChrootBase(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live-orphan sweep reads /proc; linux only")
	}
	base := t.TempDir()
	const id = "sbx-liveorphantest"
	if !validIDPattern.MatchString(id) {
		t.Fatalf("test id %q is not a valid jailer id", id)
	}

	// `sh -c "sleep 3600" sh --chroot-base-dir <base> --id <id>` keeps the
	// shell alive (waiting on sleep) with jailer's scoping tokens in its own
	// cmdline; the trailing args land in $0.. and are ignored by the script.
	cmd := exec.Command("/bin/sh", "-c", "sleep 3600", "sh",
		"--chroot-base-dir", base, "--id", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start decoy: %v", err)
	}
	pgid := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }() // reap the sleep child too

	// Wait for the kernel to publish the decoy's cmdline.
	deadline := time.Now().Add(3 * time.Second)
	for len(liveOrphanIDs(base)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("decoy never appeared in liveOrphanIDs")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Scoping: a different chroot-base must not see it.
	if got := liveOrphanIDs(t.TempDir()); len(got) != 0 {
		t.Errorf("liveOrphanIDs(other base) = %v, want empty (scoping leak)", got)
	}

	if got := liveOrphanIDs(base); len(got) != 1 || got[0] != id {
		t.Fatalf("liveOrphanIDs(base) = %v, want [%s]", got, id)
	}

	reaped := KillLiveOrphans(base)
	if len(reaped) != 1 || reaped[0] != id {
		t.Fatalf("KillLiveOrphans = %v, want [%s]", reaped, id)
	}
	if got := liveOrphanIDs(base); len(got) != 0 {
		t.Errorf("orphan still present after KillLiveOrphans: %v", got)
	}
}

// TestReapOrphanCgroups: empty per-VM cgroup dirs are removed; a non-empty
// one (a still-live VM) and a non-matching name are left in place; a missing
// root is a clean no-op.
func TestReapOrphanCgroups(t *testing.T) {
	root := t.TempDir()
	mk := func(name string) {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	mk("sbx-aaa")       // empty → reaped
	mk("sbx-bbb")       // empty → reaped
	mk("not_a_cgroup")  // invalid id (underscore) → skipped
	mk("sbx-ccc/child") // non-empty (has a child) → skipped (can't rmdir)

	reaped := reapOrphanCgroups(root)
	slices.Sort(reaped)
	if !slices.Equal(reaped, []string{"sbx-aaa", "sbx-bbb"}) {
		t.Errorf("reaped = %v, want [sbx-aaa sbx-bbb]", reaped)
	}
	for _, gone := range []string{"sbx-aaa", "sbx-bbb"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", gone)
		}
	}
	for _, kept := range []string{"not_a_cgroup", "sbx-ccc"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Errorf("%s should have been kept: %v", kept, err)
		}
	}

	// A missing cgroup root (first run / no quotas) reaps nothing, no error.
	if got := reapOrphanCgroups(filepath.Join(root, "does-not-exist")); got != nil {
		t.Errorf("missing root = %v, want nil", got)
	}
}

func TestReapOrphansNoDirIsNotError(t *testing.T) {
	// First-ever daemon startup: ChrootBase exists but has never had a
	// jailer subdirectory under it.
	reaped, err := ReapOrphans(t.TempDir(), "/usr/bin/firecracker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped on empty dir: %v", reaped)
	}
}

func TestReapOrphansSkipsNonDirEntries(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "firecracker")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A stray regular file under <base>/firecracker/ shouldn't make
	// ReapOrphans choke (jailer wouldn't create such a thing, but we
	// shouldn't crash on it either).
	if err := os.WriteFile(filepath.Join(parent, "stray.txt"), []byte("junk"), 0o640); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	seedOrphan(t, base, "firecracker", "sbx-real")

	reaped, err := ReapOrphans(base, "/usr/bin/firecracker")
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "sbx-real" {
		t.Fatalf("reaped = %v, want [sbx-real]", reaped)
	}
}
