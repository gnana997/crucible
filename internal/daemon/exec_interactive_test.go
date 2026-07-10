package daemon

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/gnana997/crucible/internal/logstore"
	"github.com/gnana997/crucible/sdk/wire"
)

// TestRelayExecFramesForwardsAndTees feeds a synthetic agent frame stream
// through relayExecFrames and asserts every frame reaches the client
// verbatim, the exit code is parsed, and stdout/stderr are teed into the
// durable log store.
func TestRelayExecFramesForwardsAndTees(t *testing.T) {
	ls, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("logstore.New: %v", err)
	}
	s := &Server{cfg: Config{LogStore: ls, Logger: logsTestLogger()}}

	// Build the "agent" side of the stream.
	var agentSide bytes.Buffer
	_ = wire.WriteFrame(&agentSide, wire.FrameStdout, []byte("out-chunk"))
	_ = wire.WriteFrame(&agentSide, wire.FrameStderr, []byte("err-chunk"))
	exitPayload, _ := json.Marshal(wire.ExecResult{ExitCode: 42})
	_ = wire.WriteFrame(&agentSide, wire.FrameExit, exitPayload)

	const id = "sbx-relaytest01"
	var clientSide bytes.Buffer
	exit := s.relayExecFrames(id, &agentSide, &clientSide)

	if exit != 42 {
		t.Errorf("exit = %d, want 42", exit)
	}

	// Client must have received all three frames unchanged.
	var gotOut, gotErr string
	sawExit := false
	for {
		f, err := wire.ReadFrame(&clientSide)
		if err != nil {
			break
		}
		switch f.Type {
		case wire.FrameStdout:
			gotOut += string(f.Payload)
		case wire.FrameStderr:
			gotErr += string(f.Payload)
		case wire.FrameExit:
			var res wire.ExecResult
			if err := json.Unmarshal(f.Payload, &res); err != nil || res.ExitCode != 42 {
				t.Errorf("forwarded exit frame = %q (err %v), want exit 42", f.Payload, err)
			}
			sawExit = true
		}
	}
	if gotOut != "out-chunk" || gotErr != "err-chunk" {
		t.Errorf("forwarded stdout=%q stderr=%q, want %q/%q", gotOut, gotErr, "out-chunk", "err-chunk")
	}
	if !sawExit {
		t.Error("client never received an exit frame")
	}

	// Output must be teed into the log store as exec records.
	recs, _, _ := ls.Read(id, -1, 1<<20, 0)
	var teeOut, teeErr string
	for _, r := range recs {
		if r.Source != logstore.SourceExec {
			continue
		}
		switch r.Stream {
		case logstore.StreamStdout:
			teeOut += r.Text
		case logstore.StreamStderr:
			teeErr += r.Text
		}
	}
	if teeOut != "out-chunk" || teeErr != "err-chunk" {
		t.Errorf("teed stdout=%q stderr=%q, want %q/%q", teeOut, teeErr, "out-chunk", "err-chunk")
	}
}

// TestRelayExecFramesNoExitFrame: a stream that ends without an exit frame
// (e.g. the agent conn dropped) reports exit -1 rather than hanging.
func TestRelayExecFramesNoExitFrame(t *testing.T) {
	s := &Server{cfg: Config{Logger: logsTestLogger()}} // no LogStore

	var agentSide bytes.Buffer
	_ = wire.WriteFrame(&agentSide, wire.FrameStdout, []byte("partial"))
	// no exit frame — stream just ends

	var clientSide bytes.Buffer
	if exit := s.relayExecFrames("sbx-noexit000001", &agentSide, &clientSide); exit != -1 {
		t.Errorf("exit = %d, want -1 for a truncated stream", exit)
	}
}
