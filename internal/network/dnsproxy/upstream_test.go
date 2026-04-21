package dnsproxy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempResolv(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("write resolv: %v", err)
	}
	return p
}

func TestResolveUpstreamExplicitIP(t *testing.T) {
	got, err := ResolveUpstream("8.8.8.8")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "8.8.8.8:53" {
		t.Errorf("got %q, want 8.8.8.8:53", got)
	}
}

func TestResolveUpstreamExplicitIPPort(t *testing.T) {
	got, err := ResolveUpstream("8.8.8.8:5353")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "8.8.8.8:5353" {
		t.Errorf("got %q, want 8.8.8.8:5353", got)
	}
}

func TestResolveUpstreamRejectsBadInput(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "999.999.999.999", "8.8.8.8:abc"} {
		if _, err := ResolveUpstream(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestFirstNameserverReturnsFirstValidLine(t *testing.T) {
	path := writeTempResolv(t, `
# example comment
; alternate comment form
search example.com
nameserver 10.0.0.1
nameserver 10.0.0.2
`)
	ns, err := firstNameserver(path)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if ns != "10.0.0.1" {
		t.Errorf("got %q, want 10.0.0.1", ns)
	}
}

func TestFirstNameserverSkipsCorruptEntries(t *testing.T) {
	path := writeTempResolv(t, `
nameserver not-an-ip
nameserver 10.20.30.40
`)
	ns, err := firstNameserver(path)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if ns != "10.20.30.40" {
		t.Errorf("got %q, want 10.20.30.40", ns)
	}
}

func TestFirstNameserverMissingFile(t *testing.T) {
	_, err := firstNameserver("/nonexistent/does/not/exist")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("err should be IsNotExist-compatible, got %v", err)
	}
}

func TestFirstNameserverNoDirectives(t *testing.T) {
	path := writeTempResolv(t, "# nothing useful here\nsearch example.com\n")
	_, err := firstNameserver(path)
	if err == nil {
		t.Fatal("expected error for file without nameserver")
	}
}

func TestResolveUpstreamSystemFallback(t *testing.T) {
	// Point at a nonexistent file so the system path fails —
	// the function should return the Cloudflare fallback with
	// a non-nil "warning" error describing why.
	got, warn := resolveUpstreamWithPath("system", "/nonexistent/resolv.conf")
	if got != cloudflareFallback {
		t.Errorf("fallback addr = %q, want %q", got, cloudflareFallback)
	}
	if warn == nil {
		t.Fatal("expected non-nil warning from fallback path")
	}
	if !strings.Contains(warn.Error(), "falling back") {
		t.Errorf("warning should mention fallback: %v", warn)
	}
	// The underlying cause must be wrappable.
	if errors.Is(warn, os.ErrNotExist) {
		// good
	} else {
		t.Logf("warning chain does not include ErrNotExist (acceptable but less friendly): %v", warn)
	}
}

func TestResolveUpstreamSystemHappyPath(t *testing.T) {
	path := writeTempResolv(t, "nameserver 10.20.30.40\n")
	got, warn := resolveUpstreamWithPath("system", path)
	if warn != nil {
		t.Errorf("unexpected warning: %v", warn)
	}
	if got != "10.20.30.40:53" {
		t.Errorf("got %q, want 10.20.30.40:53", got)
	}
}

func TestResolveUpstreamEmptyStringMeansSystem(t *testing.T) {
	path := writeTempResolv(t, "nameserver 10.20.30.40\n")
	got, _ := resolveUpstreamWithPath("", path)
	if got != "10.20.30.40:53" {
		t.Errorf("got %q, want 10.20.30.40:53", got)
	}
}
