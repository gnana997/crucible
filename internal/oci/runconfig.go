package oci

import (
	"fmt"
	"sort"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// RunConfigVersion is the current run.json schema version. Bump only
// with a migration story: converted images on disk carry the version
// they were built with.
const RunConfigVersion = 1

// RunConfig is run.json — the image's runtime contract as recorded at
// conversion time, baked into every converted rootfs at
// /crucible/run.json. The live boot path does not read it (the daemon
// computes the effective service spec host-side from the same source
// config); it exists so an artifact on disk is self-describing for
// debugging and future standalone use.
type RunConfig struct {
	// Version is RunConfigVersion at conversion time.
	Version int `json:"version"`

	// SourceRef is the fully-qualified reference the image came from
	// (e.g. "index.docker.io/library/nginx:latest"), or the
	// docker-archive marker for side-loads.
	SourceRef string `json:"source_ref"`

	// Digest is the resolved image manifest digest ("sha256:…") — the
	// platform image's digest, not a multi-arch index's.
	Digest string `json:"digest"`

	// ConvertedAtUnixMs and ConverterVersion stamp the conversion.
	// Filled by the assembler when the artifact is written.
	ConvertedAtUnixMs int64  `json:"converted_at_unix_ms,omitempty"`
	ConverterVersion  string `json:"converter_version,omitempty"`

	// Entrypoint and Cmd follow the OCI combination rule: no
	// Entrypoint → Cmd[0] is the executable; with an Entrypoint, Cmd
	// supplies default arguments.
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`

	// Env is the image's environment as declared, order preserved
	// ("KEY=VALUE" entries).
	Env []string `json:"env,omitempty"`

	// WorkingDir is the entrypoint's initial working directory; the
	// runtime creates it if absent (Docker behavior).
	WorkingDir string `json:"working_dir,omitempty"`

	// User is the raw OCI user field: "user", "uid", "user:group",
	// "uid:gid", "uid:group", or "user:gid". Resolution against
	// /etc/passwd + /etc/group happens in the guest, which is the only
	// place those files exist.
	User string `json:"user,omitempty"`

	// StopSignal is recorded verbatim ("SIGTERM", or a number as a
	// string — Docker allows both). Empty means the runtime default.
	StopSignal string `json:"stop_signal,omitempty"`

	// ExposedPorts are the image's declared ports ("8080/tcp"),
	// sorted. Advisory — hints for future ingress defaults.
	ExposedPorts []string `json:"exposed_ports,omitempty"`

	// Volumes are the image's declared mutable paths, sorted.
	// Advisory per the OCI spec — the runtime ensures they exist and
	// are writable; real volume mounts are a later feature.
	Volumes []string `json:"volumes,omitempty"`

	// Labels are the image's metadata labels, copied verbatim.
	Labels map[string]string `json:"labels,omitempty"`

	// Healthcheck is Docker's extension (absent from pure-OCI
	// builders); recorded for future health-probe seeding.
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
}

// Healthcheck mirrors Docker's HEALTHCHECK instruction with durations
// flattened to milliseconds for a stable wire form.
type Healthcheck struct {
	// Test is the probe command in Docker's encoding: first element
	// "CMD" (exec form) or "CMD-SHELL" (shell form), or ["NONE"] for
	// an explicitly disabled healthcheck.
	Test []string `json:"test,omitempty"`

	IntervalMs    int64 `json:"interval_ms,omitempty"`
	TimeoutMs     int64 `json:"timeout_ms,omitempty"`
	StartPeriodMs int64 `json:"start_period_ms,omitempty"`
	Retries       int   `json:"retries,omitempty"`
}

// extractRunConfig maps a validated image's OCI config onto the
// run.json shape. The caller has already checked the platform.
func extractRunConfig(cf *v1.ConfigFile, sourceRef, digest string) *RunConfig {
	cfg := cf.Config
	rc := &RunConfig{
		Version:    RunConfigVersion,
		SourceRef:  sourceRef,
		Digest:     digest,
		Entrypoint: append([]string(nil), cfg.Entrypoint...),
		Cmd:        append([]string(nil), cfg.Cmd...),
		Env:        append([]string(nil), cfg.Env...),
		WorkingDir: cfg.WorkingDir,
		User:       cfg.User,
		StopSignal: cfg.StopSignal,
	}
	rc.ExposedPorts = sortedKeys(cfg.ExposedPorts)
	rc.Volumes = sortedKeys(cfg.Volumes)
	if len(cfg.Labels) > 0 {
		rc.Labels = make(map[string]string, len(cfg.Labels))
		for k, v := range cfg.Labels {
			rc.Labels[k] = v
		}
	}
	if hc := cfg.Healthcheck; hc != nil && len(hc.Test) > 0 {
		rc.Healthcheck = &Healthcheck{
			Test:          append([]string(nil), hc.Test...),
			IntervalMs:    hc.Interval.Milliseconds(),
			TimeoutMs:     hc.Timeout.Milliseconds(),
			StartPeriodMs: hc.StartPeriod.Milliseconds(),
			Retries:       hc.Retries,
		}
	}
	return rc
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validatePlatform rejects anything but linux/amd64 — Firecracker on
// x86-64 KVM boots nothing else. Multi-arch indexes are resolved to
// the linux/amd64 child before this check; a single-platform image of
// the wrong arch lands here.
func validatePlatform(cf *v1.ConfigFile) error {
	if cf.OS != "linux" || cf.Architecture != "amd64" {
		return fmt.Errorf("oci: image is %s/%s; crucible requires linux/amd64", cf.OS, cf.Architecture)
	}
	return nil
}
