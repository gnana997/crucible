//go:build linux

package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestLinkStdFDs verifies the guest init creates the standard /dev/fd and
// /dev/std{in,out,err} → /proc/self/fd symlinks (a bare devtmpfs lacks them),
// which container entrypoints using bash process substitution rely on.
func TestLinkStdFDs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	linkStdFDsUnder(log, root)

	want := map[string]string{
		"dev/fd":     "/proc/self/fd",
		"dev/stdin":  "/proc/self/fd/0",
		"dev/stdout": "/proc/self/fd/1",
		"dev/stderr": "/proc/self/fd/2",
	}
	for link, target := range want {
		got, err := os.Readlink(filepath.Join(root, link))
		if err != nil {
			t.Errorf("/%s: not created: %v", link, err)
			continue
		}
		if got != target {
			t.Errorf("/%s → %q, want %q", link, got, target)
		}
	}

	// Idempotent: a second call over existing links is a no-op, not an error
	// (os.ErrExist is swallowed), leaving the links intact.
	linkStdFDsUnder(log, root)
	if got, _ := os.Readlink(filepath.Join(root, "dev/fd")); got != "/proc/self/fd" {
		t.Errorf("dev/fd changed on re-run: %q", got)
	}
}
