package cryptdev

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingRunner captures every invocation so a test can assert exact argv and
// that key material only ever travels on stdin.
type recordingRunner struct {
	calls []call
	// status/isLuks replies keyed by the queried name/file; default is failure
	// (inactive / not-LUKS), which is what most tests want.
	active map[string]bool
	isLUKS map[string]bool
}

type call struct {
	name  string
	args  []string
	stdin []byte
}

func (r *recordingRunner) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, call{name: name, args: args, stdin: append([]byte(nil), stdin...)})
	// Emulate the query commands so Open/Close/Format idempotency branches work.
	if len(args) >= 2 && args[0] == "status" {
		if r.active[args[1]] {
			return nil, nil
		}
		return nil, errors.New("inactive")
	}
	if len(args) >= 2 && args[0] == "isLuks" {
		if r.isLUKS[args[1]] {
			return nil, nil
		}
		return nil, errors.New("not luks")
	}
	return nil, nil
}

func (r *recordingRunner) last() call { return r.calls[len(r.calls)-1] }

// argvContains reports whether any argv token contains sub — used to prove a key
// never appears on the command line.
func (c call) argvContains(sub string) bool {
	for _, a := range c.args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return strings.Contains(c.name, sub)
}

func newTestEngine() (*Engine, *recordingRunner) {
	r := &recordingRunner{active: map[string]bool{}, isLUKS: map[string]bool{}}
	return &Engine{run: r.run}, r
}

func TestFormatArgvAndStdin(t *testing.T) {
	e, r := newTestEngine()
	key := []byte("this-is-a-32-byte-volume-key!!!!")
	if err := e.Format(context.Background(), "/vol/data.img", key); err != nil {
		t.Fatalf("Format: %v", err)
	}
	// isLuks probe, then luksFormat.
	c := r.last()
	if c.name != "cryptsetup" || c.args[0] != "luksFormat" {
		t.Fatalf("expected cryptsetup luksFormat, got %s %v", c.name, c.args)
	}
	if !bytes.Equal(c.stdin, key) {
		t.Fatalf("key must be passed on stdin verbatim")
	}
	if c.argvContains(string(key)) {
		t.Fatalf("SECURITY: key leaked into argv: %v", c.args)
	}
	// The cipher/keysize/kdf choices are part of the security contract.
	joined := strings.Join(c.args, " ")
	for _, want := range []string{"--type luks2", "aes-xts-plain64", "--key-size 512", "pbkdf2", "--key-file -"} {
		if !strings.Contains(joined, want) {
			t.Errorf("luksFormat argv missing %q: %v", want, c.args)
		}
	}
}

func TestFormatRefusesExistingLUKS(t *testing.T) {
	e, r := newTestEngine()
	r.isLUKS["/vol/data.img"] = true
	if err := e.Format(context.Background(), "/vol/data.img", make([]byte, 32)); err == nil {
		t.Fatal("Format must refuse to reformat an existing LUKS container")
	}
}

func TestOpenArgvStdinAndMapperPath(t *testing.T) {
	e, r := newTestEngine()
	key := []byte("another-32-byte-volume-key------")
	mapper, err := e.Open(context.Background(), "/vol/data.img", key, "crucible-vol-data")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if mapper != "/dev/mapper/crucible-vol-data" {
		t.Fatalf("mapper path = %q", mapper)
	}
	c := r.last()
	if c.args[0] != "open" {
		t.Fatalf("expected open, got %v", c.args)
	}
	if !bytes.Equal(c.stdin, key) {
		t.Fatal("key must be on stdin")
	}
	if c.argvContains(string(key)) {
		t.Fatalf("SECURITY: key leaked into argv: %v", c.args)
	}
}

func TestOpenIdempotentWhenActive(t *testing.T) {
	e, r := newTestEngine()
	r.active["crucible-vol-data"] = true
	mapper, err := e.Open(context.Background(), "/vol/data.img", make([]byte, 32), "crucible-vol-data")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if mapper != "/dev/mapper/crucible-vol-data" {
		t.Fatalf("mapper = %q", mapper)
	}
	// Only the status probe should have run — no second `open`.
	for _, c := range r.calls {
		if len(c.args) > 0 && c.args[0] == "open" {
			t.Fatal("Open must not re-open an already-active device")
		}
	}
}

func TestCloseNoStdinAndIdempotent(t *testing.T) {
	e, r := newTestEngine()
	// Inactive: Close is a no-op (only the status probe runs).
	if err := e.Close(context.Background(), "crucible-vol-data"); err != nil {
		t.Fatalf("Close inactive: %v", err)
	}
	for _, c := range r.calls {
		if len(c.args) > 0 && c.args[0] == "close" {
			t.Fatal("Close on an inactive device must be a no-op")
		}
	}
	// Active: Close runs, carries no stdin.
	r.active["crucible-vol-data"] = true
	if err := e.Close(context.Background(), "crucible-vol-data"); err != nil {
		t.Fatalf("Close active: %v", err)
	}
	c := r.last()
	if c.args[0] != "close" || c.stdin != nil {
		t.Fatalf("close should carry no stdin, got %v stdin=%v", c.args, c.stdin)
	}
}

func TestEraseArgv(t *testing.T) {
	e, r := newTestEngine()
	if err := e.Erase(context.Background(), "/vol/data.img"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	c := r.last()
	if c.args[0] != "luksErase" || !strings.Contains(strings.Join(c.args, " "), "--batch-mode") {
		t.Fatalf("expected luksErase --batch-mode, got %v", c.args)
	}
}
