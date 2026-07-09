package mcpserver

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
)

// TestWriteFilesTool drives write_files end-to-end against a stub daemon and
// asserts the tar the daemon receives carries the requested absolute paths and
// content.
func TestWriteFilesTool(t *testing.T) {
	var gotPath string
	got := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		tr := tar.NewReader(r.Body)
		files := 0
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Errorf("read tar: %v", err)
				break
			}
			b, _ := io.ReadAll(tr)
			got[h.Name] = string(b)
			if h.Typeflag == tar.TypeReg {
				files++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentwire.FilesPutResult{Files: files, Bytes: 12})
	})

	cs := connect(t, Config{Client: daemonClient(t, mux)})
	var out writeFilesOutput
	call(t, cs, "write_files", map[string]any{
		"sandbox_id": "sbx_x",
		"files": []map[string]any{
			{"path": "/work/main.py", "content": "print(1)"},
			{"path": "/work/data.txt", "content": "hi"},
		},
	}, &out)

	if gotPath != "/" {
		t.Errorf("daemon dest = %q, want / (paths carried in the tar)", gotPath)
	}
	if got["work/main.py"] != "print(1)" || got["work/data.txt"] != "hi" {
		t.Errorf("daemon received tar entries = %v", got)
	}
	if out.Files != 2 {
		t.Errorf("output.Files = %d, want 2", out.Files)
	}
}

func TestReadFileTool(t *testing.T) {
	// The daemon returns whatever bytes we stage; the client caps via max_bytes.
	var body []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandboxes/{id}/files", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	})
	cs := connect(t, Config{Client: daemonClient(t, mux)})

	t.Run("utf8", func(t *testing.T) {
		body = []byte("hello world")
		var out readFileOutput
		call(t, cs, "read_file", map[string]any{"sandbox_id": "sbx_x", "path": "/work/out.txt"}, &out)
		if out.Content != "hello world" || out.Base64 || out.Truncated {
			t.Errorf("out = %+v, want plain 'hello world'", out)
		}
	})

	t.Run("binary-base64", func(t *testing.T) {
		body = []byte{0xff, 0xfe, 0x00, 0x01}
		var out readFileOutput
		call(t, cs, "read_file", map[string]any{"sandbox_id": "sbx_x", "path": "/bin/x"}, &out)
		if !out.Base64 || out.Content != "//4AAQ==" {
			t.Errorf("out = %+v, want base64 //4AAQ==", out)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		body = []byte("abcdef")
		var out readFileOutput
		call(t, cs, "read_file", map[string]any{"sandbox_id": "sbx_x", "path": "/f", "max_bytes": 3}, &out)
		if !out.Truncated || out.Content != "abc" || out.Bytes != 3 {
			t.Errorf("out = %+v, want truncated 'abc'", out)
		}
	})
}
