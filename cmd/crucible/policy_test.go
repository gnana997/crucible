package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicy(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPolicyValidateValid(t *testing.T) {
	f := writePolicy(t, `{"operations":["read","exec"],"max_sandboxes":4,"net_allow_max":["pypi.org"]}`)
	var out, errb bytes.Buffer
	if code := run([]string{"policy", "validate", f}, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "policy OK") {
		t.Errorf("stdout = %q, want 'policy OK'", out.String())
	}
}

func TestPolicyValidateInvalid(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown op", `{"operations":["bogus"]}`, "bogus"},
		{"unknown field", `{"max_snadboxes":8}`, "max_snadboxes"},
		{"bad net pattern", `{"net_allow_max":["*"]}`, "net_allow_max"},
		{"negative cap", `{"max_fork":-1}`, "max_fork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := writePolicy(t, tc.body)
			var out, errb bytes.Buffer
			if code := run([]string{"policy", "validate", f}, &out, &errb); code == 0 {
				t.Fatalf("invalid policy should exit non-zero; stdout=%q", out.String())
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", errb.String(), tc.want)
			}
		})
	}
}

func TestPolicyValidateStdin(t *testing.T) {
	cmd := newPolicyCmd(&globalOpts{})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(`{"operations":["read"]}`))
	cmd.SetArgs([]string{"validate", "-"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stdin validate: %v", err)
	}
	if !strings.Contains(out.String(), "policy OK") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestTokenAddPolicyFailClosed(t *testing.T) {
	tf := filepath.Join(t.TempDir(), "tokens.json")
	bad := writePolicy(t, `{"operations":["nope"]}`)
	var out, errb bytes.Buffer
	code := run([]string{"daemon", "token", "add", "--token-file", tf, "--policy", bad}, &out, &errb)
	if code == 0 {
		t.Fatal("an invalid policy must fail closed — no token minted")
	}
	if !strings.Contains(errb.String(), "invalid policy") {
		t.Errorf("stderr = %q, want 'invalid policy'", errb.String())
	}
	if _, err := os.Stat(tf); err == nil {
		t.Error("no token file should be written when the policy is rejected")
	}
}

func TestTokenAddScopedAndList(t *testing.T) {
	tf := filepath.Join(t.TempDir(), "tokens.json")
	good := writePolicy(t, `{"operations":["read","exec"],"max_sandboxes":2}`)

	var out, errb bytes.Buffer
	if code := run([]string{"daemon", "token", "add", "--token-file", tf, "--name", "agent", "--policy", good, "--ttl", "1h"}, &out, &errb); code != 0 {
		t.Fatalf("add scoped: code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "scoped") || !strings.Contains(out.String(), "expires") {
		t.Errorf("add output = %q, want scoped + expires", out.String())
	}

	out.Reset()
	if code := run([]string{"daemon", "token", "list", "--token-file", tf}, &out, &errb); code != 0 {
		t.Fatalf("list: code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "scoped") || !strings.Contains(out.String(), "expires:") {
		t.Errorf("list output = %q, want scope + expiry columns", out.String())
	}
}

func TestTokenAddBadTTL(t *testing.T) {
	tf := filepath.Join(t.TempDir(), "tokens.json")
	var out, errb bytes.Buffer
	if code := run([]string{"daemon", "token", "add", "--token-file", tf, "--ttl", "banana"}, &out, &errb); code == 0 {
		t.Fatal("a malformed --ttl should fail")
	}
	if !strings.Contains(errb.String(), "ttl") {
		t.Errorf("stderr = %q, want it to mention ttl", errb.String())
	}
}

func TestPolicyShowScoped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/whoami" {
			_, _ = io.WriteString(w, `{"scoped":true,"policy":{"operations":["read","exec"],"max_sandboxes":4}}`)
		}
	}))
	defer ts.Close()
	var out, errb bytes.Buffer
	if code := run([]string{"policy", "show", "--addr", ts.URL}, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "scoped token") || !strings.Contains(out.String(), "max_sandboxes") {
		t.Errorf("stdout = %q, want scoped policy view", out.String())
	}
}

func TestPolicyShowUnscoped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"scoped":false}`)
	}))
	defer ts.Close()
	var out, errb bytes.Buffer
	if code := run([]string{"policy", "show", "--addr", ts.URL}, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "full access") {
		t.Errorf("stdout = %q, want 'full access'", out.String())
	}
}
