package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gnana997/crucible/sdk/wire"
)

// TestCommittedFixturesMatchManifest is the Go half of the conformance
// contract: every committed .bin decodes (with the real codec) to exactly
// what manifest.json promises, and every invalid fixture fails to decode.
// A TS/Python SDK test suite performs the same walk with its own codec.
func TestCommittedFixturesMatchManifest(t *testing.T) {
	raw, err := os.ReadFile("../manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v (run `make gen-fixtures`)", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.Header.Size != wire.FrameHeaderSize || m.MaxPayload != wire.MaxPayloadSize {
		t.Fatalf("manifest constants drifted from sdk/wire: %+v", m.Header)
	}

	for _, fx := range m.Fixtures {
		t.Run(fx.File, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", fx.File))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			r := bytes.NewReader(data)

			if fx.Invalid {
				for {
					if _, err := wire.ReadFrame(r); err != nil {
						if errors.Is(err, io.EOF) {
							t.Fatal("invalid fixture decoded cleanly to EOF")
						}
						return // failed as promised
					}
				}
			}

			for i, want := range fx.Frames {
				f, err := wire.ReadFrame(r)
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				sum := sha256.Sum256(f.Payload)
				if f.Type != want.TypeByte || len(f.Payload) != want.PayloadLen ||
					hex.EncodeToString(sum[:]) != want.PayloadSHA256 {
					t.Fatalf("frame %d = type %d len %d, manifest says type %d len %d",
						i, f.Type, len(f.Payload), want.TypeByte, want.PayloadLen)
				}
				if want.PayloadUTF8 != "" && string(f.Payload) != want.PayloadUTF8 {
					t.Fatalf("frame %d payload %q, manifest says %q", i, f.Payload, want.PayloadUTF8)
				}
				if f.Type == wire.FrameExit {
					var res wire.ExecResult
					if err := json.Unmarshal(f.Payload, &res); err != nil {
						t.Fatalf("frame %d: exit payload not ExecResult JSON: %v", i, err)
					}
				}
			}
			if _, err := wire.ReadFrame(r); !errors.Is(err, io.EOF) {
				t.Fatalf("stream has frames beyond the manifest (err=%v)", err)
			}
		})
	}
}

// TestGeneratorIsDeterministic regenerates into a temp dir and requires
// byte-identical output — the in-repo half of the CI codegen-drift check,
// and the guard that catches a codec change without `make gen-fixtures`.
func TestGeneratorIsDeterministic(t *testing.T) {
	tmp := t.TempDir()
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	os.Args = []string{"gen", "-out", tmp}
	main()

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 5 {
		t.Fatalf("generator produced only %d files", len(entries))
	}
	for _, e := range entries {
		fresh, err := os.ReadFile(filepath.Join(tmp, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		committed, err := os.ReadFile(filepath.Join("..", e.Name()))
		if err != nil {
			t.Fatalf("committed %s missing: %v (run `make gen-fixtures`)", e.Name(), err)
		}
		if !bytes.Equal(fresh, committed) {
			t.Errorf("%s drifted from the generator — run `make gen-fixtures` and commit", e.Name())
		}
	}
}
