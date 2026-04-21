package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// ---------------------------------------------------------------
// Mock Firecracker hybrid-vsock UDS server
// ---------------------------------------------------------------
//
// Each accepted connection: read "CONNECT <port>\n", write "OK 42\n",
// then hand the stream to an http.Server running the caller's handler.
// Good enough to exercise both the handshake code and the streaming
// frame protocol end-to-end over a real socket.

type hybridListener struct {
	raw          net.Listener
	okReply      string // defaults to "OK 42\n"
	rejectVerb   bool   // if true, reply with "ERR bad\n"
	handshakeErr atomic.Int32
}

func (l *hybridListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.raw.Accept()
		if err != nil {
			return nil, err
		}
		line, err := readHandshakeLine(conn)
		if err != nil {
			l.handshakeErr.Add(1)
			_ = conn.Close()
			continue
		}
		if !strings.HasPrefix(line, "CONNECT ") {
			l.handshakeErr.Add(1)
			_, _ = conn.Write([]byte("ERR malformed\n"))
			_ = conn.Close()
			continue
		}
		reply := l.okReply
		if l.rejectVerb {
			reply = "ERR nope\n"
		}
		if _, err := conn.Write([]byte(reply)); err != nil {
			_ = conn.Close()
			continue
		}
		if l.rejectVerb {
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}

func (l *hybridListener) Close() error   { return l.raw.Close() }
func (l *hybridListener) Addr() net.Addr { return l.raw.Addr() }

// startMockHybridServer spins up an http.Server behind a hybridListener
// on a temp UDS path. Returns the UDS path.
func startMockHybridServer(t *testing.T, handler http.Handler, opts ...func(*hybridListener)) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "fc.sock")
	raw, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	hl := &hybridListener{raw: raw, okReply: "OK 42\n"}
	for _, o := range opts {
		o(hl)
	}
	srv := &http.Server{Handler: handler, ReadTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() { _ = srv.Serve(hl); close(done) }()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return sock
}

// ---------------------------------------------------------------
// Tests
// ---------------------------------------------------------------

func TestHealthzHappy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	sock := startMockHybridServer(t, mux)

	c := NewClient(sock, 52)
	if err := c.GetHealthz(context.Background()); err != nil {
		t.Fatalf("GetHealthz: %v", err)
	}
}

func TestHealthzNonOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	sock := startMockHybridServer(t, mux)

	c := NewClient(sock, 52)
	err := c.GetHealthz(context.Background())
	if err == nil {
		t.Fatal("GetHealthz: got nil, want error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want it to mention 500", err)
	}
}

func TestHandshakeRejected(t *testing.T) {
	mux := http.NewServeMux()
	sock := startMockHybridServer(t, mux, func(l *hybridListener) { l.rejectVerb = true })

	c := NewClient(sock, 52)
	err := c.GetHealthz(context.Background())
	if err == nil {
		t.Fatal("got nil, want handshake error")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Errorf("err = %v, want to mention handshake", err)
	}
}

func TestDialMissingSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "does-not-exist.sock")
	c := NewClient(sock, 52)
	err := c.GetHealthz(context.Background())
	if err == nil {
		t.Fatal("got nil, want dial error")
	}
}

// execHandler is a fake agent that writes the frames we tell it to, so
// the client's frame-decoding path is exercised without spawning real
// commands.
type execHandler struct {
	writeFrames func(fw *agentwire.FrameWriter)
}

func (h *execHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/exec" {
		http.Error(w, "wrong route", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	fw := agentwire.NewFrameWriter(flushOnWrite{w: w, flusher: flusher})
	h.writeFrames(fw)
}

// flushOnWrite makes writes visible immediately so the client sees
// stdout before the connection closes — needed for the "exit frame
// comes last" assertions below to actually be assertions.
type flushOnWrite struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (f flushOnWrite) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil && f.flusher != nil {
		f.flusher.Flush()
	}
	return n, err
}

func TestExecHappy(t *testing.T) {
	h := &execHandler{writeFrames: func(fw *agentwire.FrameWriter) {
		_ = fw.WriteFrame(agentwire.FrameStdout, []byte("line1\n"))
		_ = fw.WriteFrame(agentwire.FrameStderr, []byte("warn\n"))
		_ = fw.WriteFrame(agentwire.FrameStdout, []byte("line2\n"))
		exit, _ := json.Marshal(agentwire.ExecResult{ExitCode: 0, DurationMs: 42})
		_ = fw.WriteFrame(agentwire.FrameExit, exit)
	}}
	sock := startMockHybridServer(t, h)

	c := NewClient(sock, 52)
	var stdout, stderr bytes.Buffer
	result, err := c.Exec(context.Background(),
		agentwire.ExecRequest{Cmd: []string{"/bin/true"}},
		&stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := stdout.String(); got != "line1\nline2\n" {
		t.Errorf("stdout = %q, want line1\\nline2\\n", got)
	}
	if got := stderr.String(); got != "warn\n" {
		t.Errorf("stderr = %q, want warn\\n", got)
	}
	if result.ExitCode != 0 || result.DurationMs != 42 {
		t.Errorf("result = %+v, want ExitCode=0 DurationMs=42", result)
	}
}

func TestExecNilWritersDiscarded(t *testing.T) {
	h := &execHandler{writeFrames: func(fw *agentwire.FrameWriter) {
		_ = fw.WriteFrame(agentwire.FrameStdout, []byte("ignored\n"))
		exit, _ := json.Marshal(agentwire.ExecResult{ExitCode: 1})
		_ = fw.WriteFrame(agentwire.FrameExit, exit)
	}}
	sock := startMockHybridServer(t, h)

	c := NewClient(sock, 52)
	result, err := c.Exec(context.Background(),
		agentwire.ExecRequest{Cmd: []string{"/bin/false"}},
		nil, nil, // both discarded
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
}

func TestExecRejectsEmptyCmd(t *testing.T) {
	// Server is never reached; client-side validation trips first.
	c := NewClient("/tmp/unused", 52)
	_, err := c.Exec(context.Background(), agentwire.ExecRequest{}, nil, nil)
	if err == nil {
		t.Fatal("got nil, want error")
	}
	if !strings.Contains(err.Error(), "Cmd is required") {
		t.Errorf("err = %v, want Cmd-required", err)
	}
}

func TestExecNonOKStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	})
	sock := startMockHybridServer(t, h)

	c := NewClient(sock, 52)
	_, err := c.Exec(context.Background(),
		agentwire.ExecRequest{Cmd: []string{"/bin/true"}},
		nil, nil,
	)
	if err == nil {
		t.Fatal("got nil, want error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want mention of 500", err)
	}
}

func TestExecStreamEndsWithoutExitFrame(t *testing.T) {
	// Simulate an agent that crashes mid-stream.
	h := &execHandler{writeFrames: func(fw *agentwire.FrameWriter) {
		_ = fw.WriteFrame(agentwire.FrameStdout, []byte("partial"))
		// no exit frame, connection closes after handler returns
	}}
	sock := startMockHybridServer(t, h)

	c := NewClient(sock, 52)
	_, err := c.Exec(context.Background(),
		agentwire.ExecRequest{Cmd: []string{"/bin/true"}},
		io.Discard, io.Discard,
	)
	if err == nil {
		t.Fatal("got nil, want error")
	}
	if !strings.Contains(err.Error(), "without exit frame") {
		t.Errorf("err = %v, want 'without exit frame'", err)
	}
}

func TestExecContextCancel(t *testing.T) {
	// Agent sits on the connection forever; client cancels ctx.
	handlerReady := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(handlerReady)
		<-r.Context().Done()
	})
	sock := startMockHybridServer(t, h)

	c := NewClient(sock, 52)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Exec(ctx, agentwire.ExecRequest{Cmd: []string{"/bin/true"}}, io.Discard, io.Discard)
		errCh <- err
	}()

	<-handlerReady
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Exec returned nil after cancel")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not return after cancel")
	}
}

// Verifies the handshake reader stops at '\n' and doesn't eat bytes
// belonging to the following stream. If this breaks, HTTP responses
// would fail to parse because their first bytes were already consumed.
func TestHandshakeReaderDoesNotOverRead(t *testing.T) {
	r := bytes.NewReader([]byte("OK 42\nHTTP/1.1 200 OK\r\n"))
	line, err := readHandshakeLine(r)
	if err != nil {
		t.Fatalf("readHandshakeLine: %v", err)
	}
	if line != "OK 42" {
		t.Errorf("line = %q, want 'OK 42'", line)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "HTTP/1.1 200 OK\r\n" {
		t.Errorf("remaining = %q; bytes past \\n were consumed", rest)
	}
}

// --- RefreshNetwork tests ---------------------------------------

func TestRefreshNetworkHappy(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /network/refresh", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	sock := startMockHybridServer(t, mux)

	c := NewClient(sock, 52)
	if err := c.RefreshNetwork(context.Background()); err != nil {
		t.Fatalf("RefreshNetwork: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("handler hit count = %d, want 1", hits.Load())
	}
}

func TestRefreshNetworkServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /network/refresh", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "dhclient acquire failed: no DHCPOFFER received", http.StatusInternalServerError)
	})
	sock := startMockHybridServer(t, mux)

	c := NewClient(sock, 52)
	err := c.RefreshNetwork(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want to mention 500", err)
	}
	if !strings.Contains(err.Error(), "DHCPOFFER") {
		t.Errorf("err = %v, want to include server's body", err)
	}
}

func TestRefreshNetworkDialFailure(t *testing.T) {
	// Point at a non-existent UDS so the hybrid-vsock dial fails.
	// Callers should get a wrapped error they can distinguish from
	// an HTTP-level failure.
	sock := filepath.Join(t.TempDir(), "does-not-exist.sock")
	c := NewClient(sock, 52)
	err := c.RefreshNetwork(context.Background())
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !strings.Contains(err.Error(), "network refresh") {
		t.Errorf("err = %v, want to mention 'network refresh'", err)
	}
}

func TestRefreshNetworkContextCancel(t *testing.T) {
	// If the caller's ctx is already canceled, the client should
	// surface the cancellation promptly rather than waiting on
	// the default handshake timeout.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /network/refresh", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	sock := startMockHybridServer(t, mux)

	c := NewClient(sock, 52)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	err := c.RefreshNetwork(ctx)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	// Don't assert on the exact error text — it varies by Go
	// runtime (net.OpError wrapping context.Canceled). We just
	// want it to surface something.
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context") {
		t.Errorf("err = %v, want context cancellation to surface", err)
	}
}
