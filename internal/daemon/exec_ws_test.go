package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/sdk/wire"
)

// registerEchoExec adds the guest agent's interactive exec endpoint to the
// stub agent mux: hijack, echo each FrameStdin payload back as FrameStdout,
// and end with a clean FrameExit when the client closes stdin. Mirrors the
// real agent's ?stdin=1 handler shape.
func registerEchoExec(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stdin") != "1" {
			http.Error(w, "expected stdin=1", http.StatusBadRequest)
			return
		}
		var req wire.ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Cmd) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n")
		fw := wire.NewFrameWriter(conn)
		for {
			f, err := wire.ReadFrame(conn)
			if err != nil {
				return
			}
			switch f.Type {
			case wire.FrameStdin:
				if err := fw.WriteFrame(wire.FrameStdout, f.Payload); err != nil {
					return
				}
			case wire.FrameStdinClose:
				payload, _ := json.Marshal(wire.ExecResult{ExitCode: 0})
				_ = fw.WriteFrame(wire.FrameExit, payload)
				return
			}
		}
	})
}

// dialExecWS opens the WebSocket exec endpoint for a sandbox.
func dialExecWS(t *testing.T, tsURL, id string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(tsURL, "http") + "/sandboxes/" + id + "/exec"
	return websocket.Dial(ctx, url, nil)
}

func TestExecWSEchoSession(t *testing.T) {
	ts, _ := newServiceTestServer(t)
	sb := createServiceTestSandbox(t, ts, `{}`)

	c, _, err := dialExecWS(t, ts.URL, sb.ID)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First message: the ExecRequest.
	reqBody, _ := json.Marshal(wire.ExecRequest{Cmd: []string{"cat"}})
	if err := c.Write(ctx, websocket.MessageText, reqBody); err != nil {
		t.Fatalf("write exec request: %v", err)
	}

	// Everything after is the frame byte stream over binary messages.
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	if err := wire.WriteFrame(nc, wire.FrameStdin, []byte("hello ws")); err != nil {
		t.Fatalf("write stdin frame: %v", err)
	}
	if err := wire.WriteFrame(nc, wire.FrameStdinClose, nil); err != nil {
		t.Fatalf("write stdin close: %v", err)
	}

	var out string
	sawExit := false
	for !sawExit {
		f, err := wire.ReadFrame(nc)
		if err != nil {
			t.Fatalf("read frame (stdout so far %q): %v", out, err)
		}
		switch f.Type {
		case wire.FrameStdout:
			out += string(f.Payload)
		case wire.FrameExit:
			var res wire.ExecResult
			if err := json.Unmarshal(f.Payload, &res); err != nil || res.ExitCode != 0 {
				t.Fatalf("exit frame = %q (err %v), want exit 0", f.Payload, err)
			}
			sawExit = true
		}
	}
	if out != "hello ws" {
		t.Errorf("echoed stdout = %q, want %q", out, "hello ws")
	}
}

func TestExecWSUnknownSandbox(t *testing.T) {
	ts, _ := newServiceTestServer(t)
	c, resp, err := dialExecWS(t, ts.URL, "sbx_aaaaaaaaaaaaa")
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("dial succeeded for unknown sandbox")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("handshake response = %+v, want 404", resp)
	}
}

func TestExecWSBadFirstMessage(t *testing.T) {
	ts, _ := newServiceTestServer(t)
	sb := createServiceTestSandbox(t, ts, `{}`)

	c, _, err := dialExecWS(t, ts.URL, sb.ID)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"cmd":[]}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err = c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation {
		t.Fatalf("read err = %v, want close with StatusPolicyViolation", err)
	}
	if !strings.Contains(ce.Reason, "cmd is required") {
		t.Errorf("close reason = %q, want cmd-required", ce.Reason)
	}
}

// TestExecWSPlainGET pins the non-upgrade behavior the OpenAPI description
// promises: a plain GET (no upgrade handshake) answers 426.
func TestExecWSPlainGET(t *testing.T) {
	ts, _ := newServiceTestServer(t)
	sb := createServiceTestSandbox(t, ts, `{}`)
	resp, err := http.Get(ts.URL + "/sandboxes/" + sb.ID + "/exec")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plain GET = %d, want 426", resp.StatusCode)
	}
}

// TestOperationForExecWS pins the policy gate: the WebSocket exec GET is
// exec-grade, while other sandbox GETs stay reads.
func TestOperationForExecWS(t *testing.T) {
	op, gated := operationFor(http.MethodGet, "/sandboxes/sbx-abc/exec")
	if !gated || op != policy.OpExec {
		t.Errorf("GET …/exec = (%v, %v), want (exec, true)", op, gated)
	}
	op, gated = operationFor(http.MethodGet, "/sandboxes/sbx-abc")
	if !gated || op != policy.OpRead {
		t.Errorf("GET …/{id} = (%v, %v), want (read, true)", op, gated)
	}
}
