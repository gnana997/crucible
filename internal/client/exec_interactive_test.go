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
	"time"

	"github.com/gnana997/crucible/sdk/wire"
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

		var req wire.ExecRequest
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
		fw := wire.NewFrameWriter(conn)
		for {
			f, err := wire.ReadFrame(buf.Reader)
			if err != nil {
				return
			}
			switch f.Type {
			case wire.FrameStdin:
				_ = fw.WriteFrame(wire.FrameStdout, f.Payload)
			case wire.FrameStdinClose:
				payload, _ := json.Marshal(wire.ExecResult{ExitCode: 0})
				_ = fw.WriteFrame(wire.FrameExit, payload)
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
		wire.ExecRequest{Cmd: []string{"sh"}}, stdin, &stdout, &stderr)
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

// TestExecInteractiveCancelUnblocks: a cancelled context (Ctrl-C) must end an
// idle interactive session promptly — the read loop can't watch ctx directly,
// so ExecInteractive closes the conn on cancel to unblock it.
func TestExecInteractiveCancelUnblocks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n")
		// Idle "shell": drain stdin, never send a frame back.
		_, _ = io.Copy(io.Discard, buf.Reader)
	}))
	defer ts.Close()
	c := New(ts.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	returned := make(chan error, 1)
	go func() {
		_, err := c.ExecInteractive(ctx, "sbx_x",
			wire.ExecRequest{Cmd: []string{"/bin/sh"}}, strings.NewReader(""), io.Discard, io.Discard)
		returned <- err
	}()

	select {
	case <-returned:
		// Returned promptly after cancel — the desired behavior.
	case <-time.After(3 * time.Second):
		t.Fatal("ExecInteractive did not return after context cancel (Ctrl-C would hang)")
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
		wire.ExecRequest{Cmd: []string{"sh"}}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("want error for 404 response, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to mention 404", err)
	}
}
