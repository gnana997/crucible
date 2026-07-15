// Package cryptdev is crucible's host-side block-encryption engine. It wraps
// cryptsetup (LUKS2) to turn a volume backing file into an encrypted container
// whose plaintext is only ever visible through a decrypted device-mapper node.
// internal/volume uses it to give each volume its own key, so a tenant's data
// can be crypto-shredded by destroying that one key.
//
// It shells out to `cryptsetup` (mirroring the nft/ip/jailer pattern in this
// codebase: auditable, and cryptsetup manages the underlying loop device itself
// when the container is a regular file). Every call passes key material on stdin
// — never on argv, never via a temp file — and the engine never logs it.
//
// The keyslot KDF is deliberately the fast pbkdf2 floor, not argon2id: a volume
// key is a 256-bit random value (see NewDEK), so KDF hardening buys nothing while
// a slow KDF would be paid on every attach and every snapshot-wake — which must
// stay sub-second. Brute-forcing a 256-bit key is infeasible regardless of KDF.
package cryptdev

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// MapperDir is where cryptsetup creates decrypted device nodes.
const MapperDir = "/dev/mapper/"

// LUKSHeaderBytes is the on-disk overhead a LUKS2 header adds to a container. A
// volume's backing file is sized this much larger than its logical size so the
// decrypted device presents the full requested capacity.
const LUKSHeaderBytes = 16 << 20 // 16 MiB

// runner executes a command with optional stdin, returning combined output. The
// engine holds one so tests can assert argv + stdin without touching a disk.
type runner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

// Engine runs the cryptsetup operations. Use New; the zero value is not usable.
type Engine struct {
	run runner
}

// New returns an Engine that shells out to the real cryptsetup.
func New() *Engine { return &Engine{run: execRunner} }

// Available reports whether cryptsetup is on PATH — preflighted at daemon start
// when encryption is enabled, so a misconfigured host fails loudly, not lazily.
func Available() error {
	if _, err := exec.LookPath("cryptsetup"); err != nil {
		return fmt.Errorf("cryptdev: cryptsetup not found on PATH (install cryptsetup): %w", err)
	}
	return nil
}

// Format initializes file as a LUKS2 container (AES-256-XTS) unlocked by key.
// file must already exist at its intended size. Refuses to reformat a file that
// is already a LUKS container, so it can never silently destroy live data.
func (e *Engine) Format(ctx context.Context, file string, key []byte) error {
	if e.IsLUKS(ctx, file) {
		return fmt.Errorf("cryptdev: %s is already a LUKS container (refusing to reformat)", file)
	}
	out, err := e.run(ctx, key,
		"cryptsetup", "luksFormat",
		"--type", "luks2",
		"--cipher", "aes-xts-plain64",
		"--key-size", "512", // AES-256-XTS uses two 256-bit keys
		"--pbkdf", "pbkdf2",
		"--pbkdf-force-iterations", "1000", // the floor: key is high-entropy, keep open fast
		"--batch-mode",
		"--key-file", "-",
		file)
	if err != nil {
		return fmt.Errorf("cryptdev: luksFormat %s: %w: %s", file, err, tail(out))
	}
	return nil
}

// Open unlocks file with key and activates /dev/mapper/<name>, returning its
// path. cryptsetup allocates the loop device for a file-backed container itself.
// Idempotent: if <name> is already active, returns its path without re-opening.
func (e *Engine) Open(ctx context.Context, file string, key []byte, name string) (string, error) {
	mapper := MapperDir + name
	if e.isActive(ctx, name) {
		return mapper, nil
	}
	out, err := e.run(ctx, key,
		"cryptsetup", "open",
		"--type", "luks2",
		"--key-file", "-",
		file, name)
	if err != nil {
		return "", fmt.Errorf("cryptdev: open %s: %w: %s", file, err, tail(out))
	}
	return mapper, nil
}

// Close deactivates the mapper device <name> and detaches the loop device
// cryptsetup created for it. Idempotent: closing an inactive name is a no-op.
func (e *Engine) Close(ctx context.Context, name string) error {
	if !e.isActive(ctx, name) {
		return nil
	}
	out, err := e.run(ctx, nil, "cryptsetup", "close", name)
	if err != nil {
		return fmt.Errorf("cryptdev: close %s: %w: %s", name, err, tail(out))
	}
	return nil
}

// Erase destroys every keyslot in file's LUKS header, making the ciphertext
// permanently unrecoverable even if the wrapped key later leaks — the on-disk
// half of crypto-shred. The device must be closed first.
func (e *Engine) Erase(ctx context.Context, file string) error {
	out, err := e.run(ctx, nil, "cryptsetup", "luksErase", "--batch-mode", file)
	if err != nil {
		return fmt.Errorf("cryptdev: luksErase %s: %w: %s", file, err, tail(out))
	}
	return nil
}

// IsLUKS reports whether file is a LUKS container. A non-LUKS or absent file is
// reported false (cryptsetup exits non-zero), not an error.
func (e *Engine) IsLUKS(ctx context.Context, file string) bool {
	_, err := e.run(ctx, nil, "cryptsetup", "isLuks", file)
	return err == nil
}

// isActive reports whether a mapper device named name is currently open.
func (e *Engine) isActive(ctx context.Context, name string) bool {
	_, err := e.run(ctx, nil, "cryptsetup", "status", name)
	return err == nil
}

// tail returns the trailing portion of command output for error context. It can
// never contain key material: keys are passed on stdin, never echoed by cryptsetup.
func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	const max = 300
	if len(s) > max {
		return "…" + s[len(s)-max:]
	}
	return s
}
