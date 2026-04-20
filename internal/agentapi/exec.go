package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
	defer resp.Body.Close()

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
