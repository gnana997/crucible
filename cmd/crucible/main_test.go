package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q; want 0", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "crucible ") {
		t.Fatalf("stdout=%q; want prefix %q", stdout.String(), "crucible ")
	}
}

func TestRunNoArgsShowsUsageAndExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("stderr=%q; want usage text", stderr.String())
	}
}

func TestRunUnknownCommandExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d; want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command: "bogus"`) {
		t.Fatalf("stderr=%q; want unknown-command message", stderr.String())
	}
}

func TestRunHelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d; want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("stdout=%q; want usage text", stdout.String())
	}
}
