package oci

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestExtractRunConfigDefensiveCopies(t *testing.T) {
	labels := map[string]string{"k": "v"}
	env := []string{"A=1"}
	cf := &v1.ConfigFile{
		OS:           "linux",
		Architecture: "amd64",
		Config:       v1.Config{Env: env, Labels: labels},
	}
	rc := extractRunConfig(cf, "ref", "sha256:x")

	labels["k"] = "mutated"
	env[0] = "A=mutated"
	if rc.Labels["k"] != "v" || rc.Env[0] != "A=1" {
		t.Errorf("extractRunConfig shares memory with the source config: %v %v", rc.Labels, rc.Env)
	}
}

func TestExtractRunConfigEmptyConfig(t *testing.T) {
	cf := &v1.ConfigFile{OS: "linux", Architecture: "amd64"}
	rc := extractRunConfig(cf, "ref", "sha256:x")
	if rc.Healthcheck != nil || rc.ExposedPorts != nil || rc.Volumes != nil || rc.Labels != nil {
		t.Errorf("empty config produced non-empty fields: %+v", rc)
	}
}

func TestExtractRunConfigSkipsEmptyHealthcheckTest(t *testing.T) {
	cf := &v1.ConfigFile{
		OS: "linux", Architecture: "amd64",
		Config: v1.Config{Healthcheck: &v1.HealthConfig{Retries: 3}},
	}
	if rc := extractRunConfig(cf, "r", "d"); rc.Healthcheck != nil {
		t.Errorf("healthcheck without a test recorded: %+v", rc.Healthcheck)
	}
}

func TestRunConfigJSONShape(t *testing.T) {
	rc := &RunConfig{
		Version:    RunConfigVersion,
		SourceRef:  "index.docker.io/library/nginx:latest",
		Digest:     "sha256:abc",
		Entrypoint: []string{"/docker-entrypoint.sh"},
		Cmd:        []string{"nginx", "-g", "daemon off;"},
		StopSignal: "SIGQUIT",
	}
	data, err := json.Marshal(rc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		`"version":1`, `"source_ref"`, `"digest"`, `"entrypoint"`, `"cmd"`, `"stop_signal"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("run.json missing %s: %s", want, s)
		}
	}
	// Round-trip.
	var back RunConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Version != 1 || back.Cmd[2] != "daemon off;" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}

func TestValidatePlatform(t *testing.T) {
	cases := []struct {
		os, arch string
		ok       bool
	}{
		{"linux", "amd64", true},
		{"linux", "arm64", false},
		{"windows", "amd64", false},
		{"", "", false},
	}
	for _, tc := range cases {
		err := validatePlatform(&v1.ConfigFile{OS: tc.os, Architecture: tc.arch})
		if (err == nil) != tc.ok {
			t.Errorf("validatePlatform(%s/%s) = %v, want ok=%v", tc.os, tc.arch, err, tc.ok)
		}
	}
}
