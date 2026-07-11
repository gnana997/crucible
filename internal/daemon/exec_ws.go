package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/gnana997/crucible/internal/logstore"
	"github.com/gnana997/crucible/internal/sandbox"
	"github.com/gnana997/crucible/sdk/wire"
)

// execWSFirstMessageTimeout bounds how long a freshly-upgraded connection
// may sit silent before sending its ExecRequest, so an idle client can't
// hold the socket open doing nothing.
const execWSFirstMessageTimeout = 30 * time.Second

// handleExecWS is the WebSocket variant of interactive exec — the
// cross-language transport (a hijacked raw TCP stream, which the Go SDK
// uses, is unreachable from fetch()-based clients and won't traverse an
// L7 gateway; WebSocket does both). The contract:
//
//   - GET /sandboxes/{id}/exec with a WebSocket upgrade handshake.
//   - The client's first message is the JSON ExecRequest.
//   - Everything after is byte-stream framed exactly like the hijacked
//     path: the concatenation of binary message payloads in each
//     direction is one wire frame stream (FrameStdin/FrameStdinClose
//     up, FrameStdout/FrameStderr/FrameExit down). Frames may split
//     across WebSocket messages — decode the concatenated stream, not
//     individual messages.
//
// Pre-upgrade failures (bad id, unknown sandbox) are plain HTTP errors on
// the handshake response; post-upgrade failures close the socket with a
// WebSocket status and a reason string.
func (s *Server) handleExecWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sandbox.IsValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid sandbox id"))
		return
	}
	if _, err := s.cfg.Manager.Get(id); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Capture the policy before the upgrade; the session outlives the
	// request machinery.
	pol := policyFor(r)

	// An interactive session runs for as long as the command does — clear
	// the server's per-request deadlines (same reason the one-shot exec
	// path does).
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the handshake error
	}
	defer func() { _ = c.CloseNow() }()
	// Frames cap payloads at MaxPayloadSize, but a client may batch many
	// frames into one WebSocket message — allow a generous multiple.
	c.SetReadLimit(1 << 20)

	// The request context is tied to the handler's HTTP machinery, which
	// the upgrade left behind; the session's lifetime is the two
	// connections themselves.
	ctx := context.Background()

	// First message: the ExecRequest.
	req, err := readExecWSRequest(ctx, c)
	if err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	if pol != nil {
		req.TimeoutSec = pol.ClampTimeout(req.TimeoutSec)
	}

	agentConn, err := s.cfg.Manager.ExecInteractive(ctx, id, req)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, fmt.Sprintf("interactive exec: %v", err))
		return
	}
	defer func() { _ = agentConn.Close() }()

	if s.cfg.LogStore != nil {
		s.appendLog(id, logstore.Record{
			TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamEvent,
			Text: "exec (interactive): " + strings.Join(req.Cmd, " "),
		})
	}

	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)

	// Client → agent: forward raw stdin frame bytes. When the client goes
	// away, close the agent conn so the guest kills the command.
	go func() {
		_, _ = io.Copy(agentConn, nc)
		_ = agentConn.Close()
	}()

	// Agent → client: parse frames (to tee output), forward each verbatim.
	exit := s.relayExecFrames(id, agentConn, nc)

	if s.cfg.LogStore != nil {
		s.appendLog(id, logstore.Record{
			TimeMs: nowMs(), Source: logstore.SourceExec, Stream: logstore.StreamEvent,
			Text: fmt.Sprintf("exit %d", exit),
		})
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// readExecWSRequest reads and validates the session's first message.
func readExecWSRequest(ctx context.Context, c *websocket.Conn) (wire.ExecRequest, error) {
	var req wire.ExecRequest
	rctx, cancel := context.WithTimeout(ctx, execWSFirstMessageTimeout)
	defer cancel()
	_, data, err := c.Read(rctx)
	if err != nil {
		return req, fmt.Errorf("read exec request: %w", err)
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return req, fmt.Errorf("invalid exec request json: %w", err)
	}
	if len(req.Cmd) == 0 {
		return req, errors.New("cmd is required")
	}
	return req, nil
}
