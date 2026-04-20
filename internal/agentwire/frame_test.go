package agentwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestRoundTripSingleFrame(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello from stdout")
	if err := WriteFrame(&buf, FrameStdout, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if got, want := buf.Len(), FrameHeaderSize+len(payload); got != want {
		t.Errorf("buf.Len = %d, want %d", got, want)
	}

	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != FrameStdout {
		t.Errorf("Type = %d, want %d", got.Type, FrameStdout)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("Payload = %q, want %q", got.Payload, payload)
	}
}

func TestRoundTripEmptyPayload(t *testing.T) {
	// Empty payloads are valid (used for trailing exit frames with empty body).
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameExit, nil); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != FrameExit {
		t.Errorf("Type = %d, want %d", got.Type, FrameExit)
	}
	if len(got.Payload) != 0 {
		t.Errorf("Payload len = %d, want 0", len(got.Payload))
	}
}

func TestReadFrameTruncatedHeader(t *testing.T) {
	// 4 bytes is not a full 8-byte header.
	_, err := ReadFrame(bytes.NewReader([]byte{1, 0, 0, 0}))
	if err == nil {
		t.Fatal("got nil, want error")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadFrameTruncatedPayload(t *testing.T) {
	// Header says payload is 10 bytes but we only give 3.
	buf := []byte{1, 0, 0, 0, 0, 0, 0, 10, 'a', 'b', 'c'}
	_, err := ReadFrame(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("got nil, want error")
	}
}

func TestReadFrameEOFBetweenFrames(t *testing.T) {
	// Clean EOF at a frame boundary should bubble up as io.EOF so
	// readers can use it as a normal end-of-stream signal.
	_, err := ReadFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestWriteFrameRejectsOversized(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, MaxPayloadSize+1)
	if err := WriteFrame(io.Discard, FrameStdout, payload); err == nil {
		t.Fatal("got nil, want error")
	}
}

func TestFrameWriterChunksLargePayloads(t *testing.T) {
	// WriteFrame rejects oversized payloads, but FrameWriter chunks
	// transparently across multiple frames.
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	big := bytes.Repeat([]byte{'y'}, MaxPayloadSize*2+1000)
	if err := fw.WriteFrame(FrameStdout, big); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	var got []byte
	for {
		f, err := ReadFrame(&buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if f.Type != FrameStdout {
			t.Errorf("frame type = %d, want stdout", f.Type)
		}
		got = append(got, f.Payload...)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("round-trip mismatch: got %d bytes, want %d", len(got), len(big))
	}
}

func TestStreamWriterInterleaving(t *testing.T) {
	// Two StreamWriters sharing one FrameWriter should produce cleanly
	// framed output with no corruption under concurrent writes.
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	stdout := fw.Stream(FrameStdout)
	stderr := fw.Stream(FrameStderr)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _ = stdout.Write([]byte("STDOUT "))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _ = stderr.Write([]byte("STDERR "))
		}
	}()
	wg.Wait()

	stdoutCount, stderrCount := 0, 0
	for {
		f, err := ReadFrame(&buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		switch f.Type {
		case FrameStdout:
			if string(f.Payload) != "STDOUT " {
				t.Errorf("stdout frame corrupted: %q", f.Payload)
			}
			stdoutCount++
		case FrameStderr:
			if string(f.Payload) != "STDERR " {
				t.Errorf("stderr frame corrupted: %q", f.Payload)
			}
			stderrCount++
		default:
			t.Errorf("unexpected frame type %d", f.Type)
		}
	}
	if stdoutCount != 100 || stderrCount != 100 {
		t.Errorf("counts: stdout=%d stderr=%d, want 100 each", stdoutCount, stderrCount)
	}
}

func TestExecResultJSONRoundTrip(t *testing.T) {
	// Sanity check for the terminal exit-frame payload shape.
	want := ExecResult{
		ExitCode:   137,
		DurationMs: 2345,
		Signal:     "SIGKILL",
		TimedOut:   true,
	}
	buf, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(buf), `"timed_out":true`) {
		t.Errorf("marshaled form missing timed_out: %s", buf)
	}
	var got ExecResult
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Errorf("round-trip: got %+v, want %+v", got, want)
	}
}
