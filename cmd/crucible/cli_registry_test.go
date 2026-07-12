package main

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func cmdWithStdin(in string) *cobra.Command {
	c := &cobra.Command{}
	c.SetIn(strings.NewReader(in))
	c.SetErr(io.Discard)
	c.SetOut(io.Discard)
	return c
}

func TestResolveRegistrySecret(t *testing.T) {
	// --password-stdin reads and trims the trailing newline.
	if s, err := resolveRegistrySecret(cmdWithStdin("tok-from-stdin\n"), "", true); err != nil || s != "tok-from-stdin" {
		t.Errorf("stdin: got %q err=%v, want tok-from-stdin", s, err)
	}

	// --password is used directly.
	if s, err := resolveRegistrySecret(cmdWithStdin(""), "explicit", false); err != nil || s != "explicit" {
		t.Errorf("password: got %q err=%v, want explicit", s, err)
	}

	// --password + --password-stdin is a conflict.
	if _, err := resolveRegistrySecret(cmdWithStdin("x"), "y", true); err == nil {
		t.Error("both --password and --password-stdin accepted; want an error")
	}

	// Empty stdin is rejected.
	if _, err := resolveRegistrySecret(cmdWithStdin(""), "", true); err == nil {
		t.Error("empty stdin accepted; want an error")
	}
}

func TestParseRegistryAuth(t *testing.T) {
	t.Setenv("CRUCIBLE_REGISTRY_AUTH", "") // isolate from the real environment

	// USER:SECRET (secret keeps colons after the first).
	if a, err := parseRegistryAuth("alice:tok:with:colons"); err != nil || a == nil ||
		a.Username != "alice" || a.Secret != "tok:with:colons" {
		t.Fatalf("user:secret → %+v err=%v", a, err)
	}
	// Token-only (empty username).
	if a, err := parseRegistryAuth(":justtoken"); err != nil || a == nil || a.Username != "" || a.Secret != "justtoken" {
		t.Fatalf("token-only → %+v err=%v", a, err)
	}
	// Empty → nil (use the daemon's stored credentials).
	if a, err := parseRegistryAuth(""); err != nil || a != nil {
		t.Errorf("empty → %+v err=%v, want nil", a, err)
	}
	// No colon / empty secret → error.
	if _, err := parseRegistryAuth("nocolon"); err == nil {
		t.Error("no colon accepted")
	}
	if _, err := parseRegistryAuth("user:"); err == nil {
		t.Error("empty secret accepted")
	}

	// Env fallback, and the explicit flag beats the env.
	t.Setenv("CRUCIBLE_REGISTRY_AUTH", "envuser:envsec")
	if a, err := parseRegistryAuth(""); err != nil || a == nil || a.Username != "envuser" || a.Secret != "envsec" {
		t.Fatalf("env fallback → %+v err=%v", a, err)
	}
	if a, err := parseRegistryAuth("flaguser:flagsec"); err != nil || a == nil || a.Username != "flaguser" {
		t.Fatalf("flag over env → %+v err=%v", a, err)
	}
}
