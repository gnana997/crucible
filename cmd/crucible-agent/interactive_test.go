//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// dialInteractiveExec opens a raw connection to an interactive /exec
// endpoint, sends the request + JSON body, and consumes the 200 status line
// and headers (byte-at-a-time, so no frame bytes are swallowed). The
// returned conn is positioned at the first response frame.
func dialInteractiveExec(t *testing.T, addr, reqJSON string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := "POST /exec?stdin=1 HTTP/1.1\r\nHost: agent\r\nContent-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(reqJSON)) + "\r\n\r\n" + reqJSON
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	var buf []byte
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(conn, one); err != nil {
			t.Fatalf("read response header: %v", err)
		}
		buf = append(buf, one[0])
		if bytes.HasSuffix(buf, []byte("\r\n\r\n")) {
			break
		}
	}
	if !bytes.HasPrefix(buf, []byte("HTTP/1.1 200")) {
		t.Fatalf("interactive exec status: %q", buf)
	}
	return conn
}

// collectInteractive reads response frames until the terminal exit frame.
func collectInteractive(t *testing.T, conn net.Conn) (stdout, stderr []byte, result agentwire.ExecResult) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer
	for {
		f, err := agentwire.ReadFrame(conn)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		switch f.Type {
		case agentwire.FrameStdout:
			stdoutBuf.Write(f.Payload)
		case agentwire.FrameStderr:
			stderrBuf.Write(f.Payload)
		case agentwire.FrameExit:
			if err := json.Unmarshal(f.Payload, &result); err != nil {
				t.Fatalf("decode exit frame: %v", err)
			}
			return stdoutBuf.Bytes(), stderrBuf.Bytes(), result
		default:
			t.Fatalf("unexpected frame type %d", f.Type)
		}
	}
}

// TestHandleExecInteractiveStdinToExit drives a shell: a command written on
// stdin produces output, and FrameStdinClose (EOF) makes the shell exit 0.
func TestHandleExecInteractiveStdinToExit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	conn := dialInteractiveExec(t, ts.Listener.Addr().String(), `{"cmd":["/bin/sh"]}`)
	defer func() { _ = conn.Close() }()

	if err := agentwire.WriteFrame(conn, agentwire.FrameStdin, []byte("echo hi\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := agentwire.WriteFrame(conn, agentwire.FrameStdinClose, nil); err != nil {
		t.Fatalf("write stdin-close: %v", err)
	}

	stdout, _, result := collectInteractive(t, conn)
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d (err=%q), want 0", result.ExitCode, result.Error)
	}
	if !bytes.Contains(stdout, []byte("hi")) {
		t.Errorf("stdout = %q, want it to contain %q", stdout, "hi")
	}
}

// TestHandleExecInteractiveSharedState is the key interactive property: a
// long-lived shell keeps state across commands, so `cd` persists into a
// later `pwd`.
func TestHandleExecInteractiveSharedState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	conn := dialInteractiveExec(t, ts.Listener.Addr().String(), `{"cmd":["/bin/sh"]}`)
	defer func() { _ = conn.Close() }()

	for _, line := range []string{"cd /tmp\n", "pwd\n"} {
		if err := agentwire.WriteFrame(conn, agentwire.FrameStdin, []byte(line)); err != nil {
			t.Fatalf("write stdin %q: %v", line, err)
		}
	}
	if err := agentwire.WriteFrame(conn, agentwire.FrameStdinClose, nil); err != nil {
		t.Fatalf("write stdin-close: %v", err)
	}

	stdout, _, result := collectInteractive(t, conn)
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !bytes.Contains(stdout, []byte("/tmp")) {
		t.Errorf("stdout = %q, want it to contain %q (cd state did not persist)", stdout, "/tmp")
	}
}

// TestHandleExecInteractiveBadJSON is rejected before any hijack, so the
// client sees a normal 4xx rather than a broken stream.
func TestHandleExecInteractiveBadJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", handleExec)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/exec?stdin=1", "application/json", bytes.NewReader([]byte(`{`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// recordWriteCloser captures pumpStdin's writes and whether stdin was closed.
type recordWriteCloser struct {
	buf    bytes.Buffer
	closed bool
}

func (r *recordWriteCloser) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *recordWriteCloser) Close() error                { r.closed = true; return nil }

// TestPumpStdinDeliversAndClosesOnStdinClose: a FrameStdinClose ends the
// pump gracefully (stdin closed, no disconnect fired), after delivering the
// preceding stdin payload.
func TestPumpStdinDeliversAndClosesOnStdinClose(t *testing.T) {
	var frames bytes.Buffer
	_ = agentwire.WriteFrame(&frames, agentwire.FrameStdin, []byte("payload"))
	_ = agentwire.WriteFrame(&frames, agentwire.FrameStdinClose, nil)

	wc := &recordWriteCloser{}
	disconnected := false
	pumpStdin(bufio.NewReader(&frames), wc, func() { disconnected = true })

	if got := wc.buf.String(); got != "payload" {
		t.Errorf("stdin got %q, want %q", got, "payload")
	}
	if !wc.closed {
		t.Error("stdin not closed after FrameStdinClose")
	}
	if disconnected {
		t.Error("onDisconnect fired on a graceful stdin-close")
	}
}

// TestPumpStdinDisconnectsOnReadError: a closed/empty stream (client gone)
// closes stdin and fires onDisconnect so the caller can kill the process.
func TestPumpStdinDisconnectsOnReadError(t *testing.T) {
	wc := &recordWriteCloser{}
	disconnected := false
	pumpStdin(bufio.NewReader(bytes.NewReader(nil)), wc, func() { disconnected = true })

	if !wc.closed {
		t.Error("stdin not closed on disconnect")
	}
	if !disconnected {
		t.Error("onDisconnect did not fire on a read error")
	}
}
