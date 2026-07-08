package main

import (
	"bytes"
	"slices"
	"testing"
)

func TestDockerBuildArgs(t *testing.T) {
	cases := []struct {
		name           string
		tag, file, ctx string
		want           []string
	}{
		{"context only", "", "", ".", []string{"build", "-q", "."}},
		{"with tag", "myapp:latest", "", "src", []string{"build", "-q", "-t", "myapp:latest", "src"}},
		{"with dockerfile", "", "docker/Dockerfile", "src", []string{"build", "-q", "-f", "docker/Dockerfile", "src"}},
		{"tag and dockerfile", "myapp", "Containerfile", ".", []string{"build", "-q", "-t", "myapp", "-f", "Containerfile", "."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dockerBuildArgs(c.tag, c.file, c.ctx)
			if !slices.Equal(got, c.want) {
				t.Errorf("dockerBuildArgs(%q,%q,%q) = %v, want %v", c.tag, c.file, c.ctx, got, c.want)
			}
		})
	}
}

// TestBuildRequiresContextArg: build takes exactly one context path.
func TestBuildRequiresContextArg(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"build"}, &out, &errb); code == 0 {
		t.Error("build with no context arg should fail")
	}
}
