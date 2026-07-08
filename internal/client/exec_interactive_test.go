package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
)

// TestExecInteractiveRoundTrip drives client.ExecInteractive against a
// hijacking fake daemon: it verifies the request shape (stdin=1, path, auth
// header), echoes stdin frames back as stdout, and returns a clean exit.
func TestExecInteractiveRoundTrip(t *testing.T) {
	gotAuth := make(chan string, 1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stdin") != "1" {
			http.Error(w, "expected stdin=1", http.StatusBadRequest)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/exec") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		gotAuth <- r.Header.Get("Authorization")

		var req agentwire.ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Cmd) == 0 {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}

		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n")
		fw := agentwire.NewFrameWriter(conn)
		for {
			f, err := agentwire.ReadFrame(buf.Reader)
			if err != nil {
				return
			}
			switch f.Type {
			case agentwire.FrameStdin:
				_ = fw.WriteFrame(agentwire.FrameStdout, f.Payload)
			case agentwire.FrameStdinClose:
				payload, _ := json.Marshal(agentwire.ExecResult{ExitCode: 0})
				_ = fw.WriteFrame(agentwire.FrameExit, payload)
				return
			}
		}
	}

	ts := httptest.NewServer(http.HandlerFunc(handler))
	defer ts.Close()
	c := New(ts.URL, WithToken("secret-key"))

	stdin := strings.NewReader("hello-shell\n")
	var stdout, stderr bytes.Buffer
	res, err := c.ExecInteractive(context.Background(), "sbx-clienttest01",
		agentwire.ExecRequest{Cmd: []string{"sh"}}, stdin, &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecInteractive: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), "hello-shell") {
		t.Errorf("stdout = %q, want it to contain %q", stdout.String(), "hello-shell")
	}
	if auth := <-gotAuth; auth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer secret-key")
	}
}

// TestExecInteractivePreStreamError surfaces a non-200 daemon response as a
// plain error, not a broken stream.
func TestExecInteractivePreStreamError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such sandbox", http.StatusNotFound)
	}))
	defer ts.Close()
	c := New(ts.URL)

	_, err := c.ExecInteractive(context.Background(), "sbx-missing00001",
		agentwire.ExecRequest{Cmd: []string{"sh"}}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("want error for 404 response, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to mention 404", err)
	}
}
