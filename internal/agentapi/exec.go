package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

// maxErrorBody caps how much of a non-2xx response body we include in
// the returned error message. The body is expected to be a short
// agent-side JSON error; 8 KiB is generous.
const maxErrorBody = 8 << 10

// Exec runs a command inside the sandbox via the guest agent and
// streams its stdout/stderr to the provided writers as the command
// runs. Returns the final ExecResult parsed from the terminal exit
// frame.
//
// stdout and stderr may be nil; output for a nil writer is discarded.
// The stream is written frame-by-frame, so if stdout implements
// http.Flusher (e.g. via a wrapper), the daemon can forward bytes to
// its own HTTP client with byte-level latency.
//
// Cancelling ctx terminates the underlying connection; the agent
// notices and kills the command (its handler observes r.Context done).
func (c *Client) Exec(
	ctx context.Context,
	req agentwire.ExecRequest,
	stdout, stderr io.Writer,
) (agentwire.ExecResult, error) {
	if len(req.Cmd) == 0 {
		return agentwire.ExecResult{}, errors.New("agentapi: ExecRequest.Cmd is required")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	body, err := json.Marshal(req)
	if err != nil {
		return agentwire.ExecResult{}, fmt.Errorf("agentapi: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/exec", bytes.NewReader(body))
	if err != nil {
		return agentwire.ExecResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/octet-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return agentwire.ExecResult{}, fmt.Errorf("agentapi: exec: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Headers came back non-2xx — no streamed body, just a JSON
		// error or plain text from the agent. Surface it verbatim.
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return agentwire.ExecResult{}, fmt.Errorf(
			"agentapi: exec returned %d: %s",
			resp.StatusCode, bytes.TrimSpace(limited),
		)
	}

	return readFramedStream(resp.Body, stdout, stderr)
}

// ExecInteractive opens a full-duplex framed exec session to the guest
// agent and returns the raw connection positioned at the first response
// frame. Unlike Exec (a buffered net/http round-trip that cannot flush
// per-keystroke stdin), it dials the vsock directly, writes
// POST /exec?stdin=1 with the JSON ExecRequest, reads the response status,
// and hands the caller the conn.
//
// The caller owns the returned conn: write FrameStdin / FrameStdinClose
// frames to it, read FrameStdout / FrameStderr / FrameExit frames from it,
// and Close it when done. Closing the conn terminates the command on the
// guest (its handler observes the read error).
func (c *Client) ExecInteractive(ctx context.Context, req agentwire.ExecRequest) (net.Conn, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("agentapi: ExecRequest.Cmd is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("agentapi: marshal: %w", err)
	}

	conn, err := c.dial(ctx, "", "")
	if err != nil {
		return nil, err
	}

	// Bound the request/response exchange; the dial cleared the handshake
	// deadline, so re-arm it here and clear it again once the stream is up
	// so the session can run for as long as the command takes.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(DefaultHandshakeTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}

	var hdr bytes.Buffer
	hdr.WriteString("POST /exec?stdin=1 HTTP/1.1\r\n")
	hdr.WriteString("Host: agent\r\n")
	hdr.WriteString("Content-Type: application/json\r\n")
	fmt.Fprintf(&hdr, "Content-Length: %d\r\n", len(body))
	// Connection: close so a pre-stream error response (not hijacked) ends
	// with EOF rather than lingering on keep-alive.
	hdr.WriteString("Connection: close\r\n")
	hdr.WriteString("\r\n")
	hdr.Write(body)
	if _, err := conn.Write(hdr.Bytes()); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("agentapi: write exec request: %w", err)
	}

	status, err := readStatusCode(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("agentapi: read exec response: %w", err)
	}
	if status != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("agentapi: interactive exec returned %d", status)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// maxHeaderLine caps a single status/header line read during the
// interactive-exec response parse. Real lines are tiny; this is defensive.
const maxHeaderLine = 8 << 10

// readStatusCode reads an HTTP/1.1 status line and headers one byte at a
// time — never over-reading into the frame stream that follows — and
// returns the numeric status code. Deliberately not bufio, which would
// buffer past the header terminator and swallow frame bytes.
func readStatusCode(r io.Reader) (int, error) {
	statusLine, err := readCRLFLine(r)
	if err != nil {
		return 0, err
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0, fmt.Errorf("malformed status line %q", statusLine)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("malformed status code %q", parts[1])
	}
	for {
		line, err := readCRLFLine(r)
		if err != nil {
			return 0, err
		}
		if line == "" {
			break
		}
	}
	return code, nil
}

// readCRLFLine reads until '\n', returning the line without a trailing
// '\r'. Byte-at-a-time so no bytes past the line are consumed.
func readCRLFLine(r io.Reader) (string, error) {
	buf := make([]byte, 0, 64)
	var one [1]byte
	for len(buf) < maxHeaderLine {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return strings.TrimSuffix(string(buf), "\r"), nil
		}
		buf = append(buf, one[0])
	}
	return "", errors.New("header line exceeded maximum length")
}

// readFramedStream consumes an agentwire frame stream, dispatching
// stdout / stderr bytes to the given writers and returning the final
// ExecResult from the exit frame. Unknown frame types are ignored for
// forward compatibility.
func readFramedStream(r io.Reader, stdout, stderr io.Writer) (agentwire.ExecResult, error) {
	var result agentwire.ExecResult
	sawExit := false

	for {
		f, err := agentwire.ReadFrame(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, fmt.Errorf("agentapi: read frame: %w", err)
		}
		switch f.Type {
		case agentwire.FrameStdout:
			if _, err := stdout.Write(f.Payload); err != nil {
				return result, fmt.Errorf("agentapi: write stdout: %w", err)
			}
		case agentwire.FrameStderr:
			if _, err := stderr.Write(f.Payload); err != nil {
				return result, fmt.Errorf("agentapi: write stderr: %w", err)
			}
		case agentwire.FrameExit:
			if err := json.Unmarshal(f.Payload, &result); err != nil {
				return result, fmt.Errorf("agentapi: decode exit frame: %w", err)
			}
			sawExit = true
		default:
			// Forward-compatible: ignore frame types we don't know yet.
		}
	}

	if !sawExit {
		return result, errors.New("agentapi: stream ended without exit frame")
	}
	return result, nil
}
