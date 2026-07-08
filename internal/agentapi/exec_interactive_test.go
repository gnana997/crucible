package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
)

// echoInteractiveHandler mimics the guest agent's interactive exec handler:
// hijack, echo each FrameStdin payload back as FrameStdout, and end with a
// clean FrameExit when the client closes stdin.
func echoInteractiveHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stdin") != "1" {
			http.Error(w, "expected stdin=1", http.StatusBadRequest)
			return
		}
		var req agentwire.ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Cmd) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
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
}

func TestExecInteractiveDuplex(t *testing.T) {
	sock := startMockHybridServer(t, echoInteractiveHandler(t))
	c := NewClient(sock, 42)

	conn, err := c.ExecInteractive(context.Background(), agentwire.ExecRequest{Cmd: []string{"sh"}})
	if err != nil {
		t.Fatalf("ExecInteractive: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := agentwire.WriteFrame(conn, agentwire.FrameStdin, []byte("ping")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	echo, err := agentwire.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if echo.Type != agentwire.FrameStdout || string(echo.Payload) != "ping" {
		t.Errorf("echo = (type %d, %q), want (stdout, %q)", echo.Type, echo.Payload, "ping")
	}

	if err := agentwire.WriteFrame(conn, agentwire.FrameStdinClose, nil); err != nil {
		t.Fatalf("write stdin-close: %v", err)
	}
	exit, err := agentwire.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read exit: %v", err)
	}
	if exit.Type != agentwire.FrameExit {
		t.Fatalf("frame type = %d, want exit", exit.Type)
	}
	var res agentwire.ExecResult
	if err := json.Unmarshal(exit.Payload, &res); err != nil {
		t.Fatalf("decode exit: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestExecInteractiveRejectsEmptyCmd(t *testing.T) {
	c := NewClient("/nonexistent.sock", 42)
	if _, err := c.ExecInteractive(context.Background(), agentwire.ExecRequest{}); err == nil {
		t.Fatal("want error for empty cmd, got nil")
	}
}

func TestReadStatusCodeNoOverRead(t *testing.T) {
	// A well-formed 200 response followed immediately by a frame: the parser
	// must stop exactly at the header terminator so the frame is intact.
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n")
	if err := agentwire.WriteFrame(&b, agentwire.FrameStdout, []byte("after-headers")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	r := bytes.NewReader(b.Bytes())
	code, err := readStatusCode(r)
	if err != nil {
		t.Fatalf("readStatusCode: %v", err)
	}
	if code != 200 {
		t.Errorf("code = %d, want 200", code)
	}
	f, err := agentwire.ReadFrame(r)
	if err != nil {
		t.Fatalf("ReadFrame after headers: %v", err)
	}
	if f.Type != agentwire.FrameStdout || string(f.Payload) != "after-headers" {
		t.Errorf("leftover frame = (type %d, %q), want (stdout, %q)", f.Type, f.Payload, "after-headers")
	}
}

func TestReadStatusCodeNonOK(t *testing.T) {
	code, err := readStatusCode(bytes.NewReader([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n")))
	if err != nil {
		t.Fatalf("readStatusCode: %v", err)
	}
	if code != 502 {
		t.Errorf("code = %d, want 502", code)
	}
}

func TestReadStatusCodeMalformed(t *testing.T) {
	if _, err := readStatusCode(bytes.NewReader([]byte("garbage\r\n\r\n"))); err == nil {
		t.Fatal("want error for malformed status line, got nil")
	}
}
