package fsutil

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	want := []byte("hello world — reflink or fallback, we don't care which path runs")
	if err := os.WriteFile(src, want, 0o640); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := Clone(src, dst); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestCloneOverwritesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, []byte("new-short"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old content that is much longer than the new content"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := Clone(src, dst); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-short" {
		t.Errorf("dst = %q, want 'new-short' (should be truncated to src size)", got)
	}
}

func TestCloneEmptyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Clone(src, dst); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("dst size = %d, want 0", info.Size())
	}
}

func TestCloneSrcMissing(t *testing.T) {
	dir := t.TempDir()
	err := Clone(filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Fatal("Clone: got nil, want error for missing src")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want wraps os.ErrNotExist", err)
	}
}

func TestCloneDstParentMissing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Parent directory of dst doesn't exist → open for create fails.
	err := Clone(src, filepath.Join(dir, "does-not-exist", "dst"))
	if err == nil {
		t.Fatal("Clone: got nil, want error for missing dst parent")
	}
}

func TestCloneLargeFile(t *testing.T) {
	// Reasonable proxy for a rootfs-sized payload — exercises the
	// io.Copy fallback with a non-trivial amount of data. Kept under
	// a few MB to keep the test cheap.
	dir := t.TempDir()
	src := filepath.Join(dir, "big")
	dst := filepath.Join(dir, "big.clone")

	payload := bytes.Repeat([]byte("crucible-"), 1<<17) // ~1.15 MB
	if err := os.WriteFile(src, payload, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Clone(src, dst); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch on large file (got %d bytes, want %d)", len(got), len(payload))
	}
}
