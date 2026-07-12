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
