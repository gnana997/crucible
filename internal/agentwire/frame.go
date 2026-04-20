package agentwire

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Frame kinds. Keep numeric values stable — they travel on the wire.
const (
	FrameStdout byte = 1
	FrameStderr byte = 2
	FrameExit   byte = 3
)

// FrameHeaderSize is the fixed-width header that precedes every frame's
// payload: 1 byte type + 3 bytes reserved + 4 bytes big-endian size.
const FrameHeaderSize = 8

// MaxPayloadSize bounds a single frame's payload. Larger writes are split
// across multiple frames transparently by StreamWriter. Keeps a single
// frame small enough to fit comfortably in most TCP/VSOCK buffers.
const MaxPayloadSize = 64 * 1024

// Frame is a parsed response frame.
type Frame struct {
	Type    byte
	Payload []byte
}

// WriteFrame serializes one frame to w. Callers that share one w across
// multiple goroutines must serialize access (FrameWriter does this).
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxPayloadSize {
		return fmt.Errorf("agentwire: payload %d > MaxPayloadSize %d", len(payload), MaxPayloadSize)
	}
	var hdr [FrameHeaderSize]byte
	hdr[0] = typ
	// hdr[1..4] reserved, left zero
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame consumes exactly one frame from r. Returns io.EOF only when
// r is fully closed between frames — a partial frame is reported as
// io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [FrameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	typ := hdr[0]
	size := binary.BigEndian.Uint32(hdr[4:])
	if size > MaxPayloadSize {
		return Frame{}, fmt.Errorf("agentwire: frame size %d > MaxPayloadSize %d", size, MaxPayloadSize)
	}
	payload := make([]byte, size)
	if size > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Type: typ, Payload: payload}, nil
}

// FrameWriter serializes frame writes to an underlying io.Writer. Multiple
// goroutines may call WriteFrame or use StreamWriter in parallel — the
// internal mutex ensures frames are never interleaved mid-write.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter wraps w. Write ordering to w is serialized through the
// returned FrameWriter; don't continue writing to w directly afterward.
func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{w: w}
}

// WriteFrame writes one frame, chunking oversize payloads across multiple
// frames of MaxPayloadSize each. All frames in the chunk carry the same
// type byte — consumers must treat them as belonging to the same logical
// stream (stdout/stderr/...).
func (fw *FrameWriter) WriteFrame(typ byte, payload []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	for len(payload) > MaxPayloadSize {
		if err := WriteFrame(fw.w, typ, payload[:MaxPayloadSize]); err != nil {
			return err
		}
		payload = payload[MaxPayloadSize:]
	}
	return WriteFrame(fw.w, typ, payload)
}

// Stream returns an io.Writer that writes every Write as one or more
// frames of the given type. Returned writers share fw's mutex, so
// concurrent writes to stdout + stderr streams are safe.
func (fw *FrameWriter) Stream(typ byte) io.Writer {
	return &streamWriter{fw: fw, typ: typ}
}

type streamWriter struct {
	fw  *FrameWriter
	typ byte
}

// Write implements io.Writer. Always returns (len(p), nil) on success.
func (sw *streamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := sw.fw.WriteFrame(sw.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
