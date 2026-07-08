package daemon

import (
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
	"github.com/gnana997/crucible/internal/oci"
)

func TestEffectiveServiceSpecImageOnly(t *testing.T) {
	rc := &oci.RunConfig{
		Entrypoint: []string{"/docker-entrypoint.sh"},
		Cmd:        []string{"nginx", "-g", "daemon off;"},
		Env:        []string{"PATH=/custom", "NGINX_VERSION=1.27"},
		WorkingDir: "/app",
		User:       "nginx",
		StopSignal: "SIGQUIT",
	}
	spec := effectiveServiceSpec(rc, nil)
	if spec == nil {
		t.Fatal("nil spec for an image with a command")
	}
	want := []string{"/docker-entrypoint.sh", "nginx", "-g", "daemon off;"}
	if len(spec.Cmd) != len(want) {
		t.Fatalf("cmd = %v, want %v", spec.Cmd, want)
	}
	for i := range want {
		if spec.Cmd[i] != want[i] {
			t.Fatalf("cmd = %v, want %v", spec.Cmd, want)
		}
	}
	if !spec.EnvExact {
		t.Error("EnvExact must be true for image services")
	}
	if spec.Cwd != "/app" || spec.User != "nginx" || spec.StopSignal != "SIGQUIT" {
		t.Errorf("cwd/user/stop = %q %q %q", spec.Cwd, spec.User, spec.StopSignal)
	}
	if spec.Env["PATH"] != "/custom" || spec.Env["NGINX_VERSION"] != "1.27" {
		t.Errorf("env = %v", spec.Env)
	}
}

func TestEffectiveServiceSpecOverrideReplacesCmd(t *testing.T) {
	rc := &oci.RunConfig{
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"default"},
		Env:        []string{"A=1", "B=2"},
		User:       "root",
	}
	override := &agentwire.ServiceSpec{
		Cmd:     []string{"/bin/sh", "-c", "echo hi"},
		Env:     map[string]string{"B": "override", "C": "3"},
		User:    "1000:1000",
		Restart: agentwire.RestartPolicy{Policy: agentwire.RestartAlways},
	}
	spec := effectiveServiceSpec(rc, override)
	if len(spec.Cmd) != 3 || spec.Cmd[0] != "/bin/sh" {
		t.Errorf("override cmd not applied: %v", spec.Cmd)
	}
	// Env: image A=1 kept, B overridden, C added.
	if spec.Env["A"] != "1" || spec.Env["B"] != "override" || spec.Env["C"] != "3" {
		t.Errorf("env merge wrong: %v", spec.Env)
	}
	if spec.User != "1000:1000" {
		t.Errorf("user = %q, want the override", spec.User)
	}
	if spec.Restart.Policy != agentwire.RestartAlways {
		t.Errorf("restart policy not carried from override: %+v", spec.Restart)
	}
}

func TestEffectiveServiceSpecNoCommandIsNil(t *testing.T) {
	// An image with no entrypoint/cmd and no override → bare sandbox.
	if spec := effectiveServiceSpec(&oci.RunConfig{Env: []string{"X=1"}}, nil); spec != nil {
		t.Errorf("spec = %+v, want nil (nothing to run)", spec)
	}
	// nil run config + nil override → nil.
	if spec := effectiveServiceSpec(nil, nil); spec != nil {
		t.Errorf("spec = %+v, want nil", spec)
	}
	// A cmd-only override against an entrypoint-less image runs.
	spec := effectiveServiceSpec(nil, &agentwire.ServiceSpec{Cmd: []string{"/bin/true"}})
	if spec == nil || spec.Cmd[0] != "/bin/true" {
		t.Errorf("override-only spec = %+v", spec)
	}
}

func TestNormalizeStopSignal(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"SIGTERM": "SIGTERM",
		"SIGQUIT": "SIGQUIT",
		"9":       "SIGKILL",
		"15":      "SIGTERM",
		"notanum": "notanum",
	}
	for in, want := range cases {
		if got := normalizeStopSignal(in); got != want {
			t.Errorf("normalizeStopSignal(%q) = %q, want %q", in, got, want)
		}
	}
}
