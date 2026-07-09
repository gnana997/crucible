package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestParseCpArg(t *testing.T) {
	cases := []struct {
		in         string
		wantID     string
		wantPath   string
		wantRemote bool
	}{
		{"sbx_abc123:/work", "sbx_abc123", "/work", true},
		{"sbx_abc123:/a/b.txt", "sbx_abc123", "/a/b.txt", true},
		{"./local.py", "", "./local.py", false},
		{"/abs/path", "", "/abs/path", false},
		{"sbx_abc123", "", "sbx_abc123", false}, // no colon → local
		{"notasbx:/x", "", "notasbx:/x", false}, // wrong prefix → local
	}
	for _, tc := range cases {
		id, path, remote := parseCpArg(tc.in)
		if id != tc.wantID || path != tc.wantPath || remote != tc.wantRemote {
			t.Errorf("parseCpArg(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, id, path, remote, tc.wantID, tc.wantPath, tc.wantRemote)
		}
	}
}

func TestTarLocalPath_RoundTrip(t *testing.T) {
	// A small tree: dir with a file and a subdir.
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(filepath.Join(proj, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "main.py"), []byte("print(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "sub", "b.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := tarLocalPath(proj, &buf); err != nil {
		t.Fatalf("tarLocalPath: %v", err)
	}

	// Entry names must be basename-relative (proj/...), so a later extract
	// beneath a dest lands the tree under <dest>/proj.
	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			got[h.Name] = string(b)
		} else {
			got[h.Name] = ""
		}
	}
	for _, want := range []string{"proj/", "proj/main.py", "proj/sub/", "proj/sub/b.txt"} {
		if _, ok := got[want]; !ok {
			t.Errorf("tar missing entry %q (got %v)", want, tarKeys(got))
		}
	}
	if got["proj/main.py"] != "print(1)" || got["proj/sub/b.txt"] != "hi" {
		t.Errorf("tar contents wrong: %v", got)
	}
}

func TestTarLocalPath_SingleFile(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "hello.py")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tarLocalPath(f, &buf); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "hello.py" {
		t.Errorf("single-file entry name = %q, want hello.py", h.Name)
	}
}

func tarKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
