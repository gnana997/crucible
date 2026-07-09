//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// buildTar assembles an in-memory tar from a list of entries.
func buildTar(t *testing.T, entries []*tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write header %s: %v", h.Name, err)
		}
		// Only regular files carry a body; a body after a symlink/dir header
		// would exceed the declared size and corrupt the stream.
		if h.Typeflag == tar.TypeReg {
			if b, ok := bodies[h.Name]; ok {
				if _, err := tw.Write([]byte(b)); err != nil {
					t.Fatalf("write body %s: %v", h.Name, err)
				}
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func TestExtractTarInto_HappyPath(t *testing.T) {
	dest := t.TempDir()
	raw := buildTar(t,
		[]*tar.Header{
			{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "app/main.py", Typeflag: tar.TypeReg, Mode: 0o644, Size: 11},
			{Name: "app/link", Typeflag: tar.TypeSymlink, Linkname: "main.py"},
		},
		map[string]string{"app/main.py": "print(1)\n#x"},
	)
	res, err := extractTarInto(dest, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.Files != 1 || res.Bytes != 11 {
		t.Errorf("summary = %+v, want 1 file / 11 bytes", res)
	}
	got, err := os.ReadFile(filepath.Join(dest, "app/main.py"))
	if err != nil || string(got) != "print(1)\n#x" {
		t.Errorf("file content = %q (%v)", got, err)
	}
	if fi, err := os.Lstat(filepath.Join(dest, "app/link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("in-dest symlink not created: %v", err)
	}
}

func TestExtractTarInto_RejectsTraversal(t *testing.T) {
	cases := []struct {
		name  string
		entry *tar.Header
	}{
		{"dotdot", &tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3}},
		{"nested-dotdot", &tar.Header{Name: "a/../../evil.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3}},
		{"escaping-symlink", &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"}},
		{"absolute-symlink", &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			raw := buildTar(t, []*tar.Header{tc.entry}, map[string]string{tc.entry.Name: "xxx"})
			if _, err := extractTarInto(dest, bytes.NewReader(raw)); err == nil {
				t.Fatalf("expected rejection for %s, got nil", tc.name)
			}
			// Nothing must have been written outside dest.
			if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "evil.txt")); err == nil {
				t.Errorf("traversal wrote a file outside dest")
			}
		})
	}
}

func TestSafeJoin(t *testing.T) {
	dest := "/srv/work"
	ok := []string{"a.txt", "sub/b.txt", "./c.txt"}
	for _, n := range ok {
		if _, good := safeJoin(dest, n); !good {
			t.Errorf("safeJoin(%q) = unsafe, want safe", n)
		}
	}
	bad := []string{"../x", "a/../../x", "../../etc/passwd"}
	for _, n := range bad {
		if _, good := safeJoin(dest, n); good {
			t.Errorf("safeJoin(%q) = safe, want unsafe", n)
		}
	}
}
