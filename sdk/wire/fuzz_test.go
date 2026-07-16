package wire

import (
	"bytes"
	"testing"
)

// FuzzReadFrame feeds arbitrary bytes to the frame reader — frames arrive from
// the guest over vsock, so a malformed or hostile header must never panic or
// over-allocate; it may only return a frame or an error.
func FuzzReadFrame(f *testing.F) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, 7, []byte("hello"))
	f.Add(buf.Bytes())
	f.Add([]byte{})
	f.Add([]byte{0x01, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}) // claims a ~4 GiB payload
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			return
		}
		// A successfully parsed frame's payload is bounded and must round-trip.
		if len(fr.Payload) > MaxPayloadSize {
			t.Fatalf("payload %d exceeds MaxPayloadSize %d", len(fr.Payload), MaxPayloadSize)
		}
		var out bytes.Buffer
		if err := WriteFrame(&out, fr.Type, fr.Payload); err != nil {
			t.Fatalf("re-encode a parsed frame: %v", err)
		}
	})
}
