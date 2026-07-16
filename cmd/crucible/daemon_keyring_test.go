//go:build linux

package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildVolumeKeyring(t *testing.T) {
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	k := func(n byte) []byte {
		x := make([]byte, 32)
		for i := range x {
			x[i] = n
		}
		return x
	}

	t.Setenv("CRUCIBLE_VOLUME_KEY", b64(k(1)))      // the default key
	t.Setenv("CRUCIBLE_VOLUME_KEY_prod", b64(k(2))) // an additional key via env
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "arch.key"), []byte(b64(k(3))), 0o600); err != nil {
		t.Fatal(err)
	}

	ring, generated, err := buildVolumeKeyring("", dir)
	if err != nil {
		t.Fatalf("buildVolumeKeyring: %v", err)
	}
	if generated {
		t.Fatal("no key should be generated when CRUCIBLE_VOLUME_KEY is set")
	}
	if string(ring["default"]) != string(k(1)) {
		t.Fatal("default key (from CRUCIBLE_VOLUME_KEY) missing/wrong")
	}
	if string(ring["prod"]) != string(k(2)) {
		t.Fatal("env key CRUCIBLE_VOLUME_KEY_prod missing/wrong")
	}
	if string(ring["arch"]) != string(k(3)) {
		t.Fatal("key-dir key arch.key missing/wrong")
	}
	if len(ring) != 3 {
		t.Fatalf("keyring size = %d, want 3", len(ring))
	}
}

func TestBuildVolumeKeyringRejectsBadKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.key"), []byte("not-base64!!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildVolumeKeyring("", dir); err == nil {
		t.Fatal("a malformed key file must surface an error, not be silently skipped")
	}
}
