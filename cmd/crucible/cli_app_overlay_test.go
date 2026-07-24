package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// overlayFor parses args against a fresh spec-flag set and overlays them onto
// base, as `app update` does. Only flag paths that don't call the daemon
// (image resolution, --secrets-from) are exercised here.
func overlayFor(t *testing.T, base api.AppSpec, args ...string) (api.AppSpec, error) {
	t.Helper()
	var opts appSpecOpts
	cmd := &cobra.Command{Use: "update"}
	opts.register(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return opts.overlay(cmd, &globalOpts{}, base)
}

func baseSpec() api.AppSpec {
	return api.AppSpec{
		Name:      "web",
		Image:     &api.ImageRef{OCI: "nginx:alpine"},
		Pull:      "missing",
		MemoryMiB: 512,
		Env:       map[string]string{"MODE": "prod"},
		Publish:   []api.PortMapping{{HostIP: "127.0.0.1", HostPort: 22379, GuestPort: 2379}},
		Restart:   wire.RestartPolicy{Policy: wire.RestartAlways},
		Health:    &api.HealthCheck{Type: "tcp", Port: 2379},
		Sleep:     &api.SleepPolicy{MinScale: 1},
		Network:   &api.NetworkRequest{Enabled: true, Allowlist: []string{"pypi.org"}},
	}
}

func TestOverlayUnsetFlagsKeepCurrentSpec(t *testing.T) {
	got, err := overlayFor(t, baseSpec(), "--idle-timeout", "30m")
	if err != nil {
		t.Fatal(err)
	}
	want := baseSpec()
	// Only the sleep policy moves; MinScale is preserved from the current value.
	if got.Sleep == nil || got.Sleep.IdleTimeoutSec != 1800 || got.Sleep.MinScale != 1 {
		t.Errorf("sleep = %+v, want idle 1800 with min_scale 1 kept", got.Sleep)
	}
	// Everything else — including instance-defining fields the flag defaults
	// would zero — is untouched.
	if got.MemoryMiB != want.MemoryMiB || got.Pull != want.Pull ||
		got.Image == nil || got.Image.OCI != want.Image.OCI ||
		got.Restart.Policy != wire.RestartAlways ||
		got.Env["MODE"] != "prod" || len(got.Publish) != 1 ||
		got.Health == nil || got.Health.Port != 2379 ||
		got.Network == nil || len(got.Network.Allowlist) != 1 {
		t.Errorf("unset flags changed the spec: %+v", got)
	}
}

func TestOverlayListFlagReplacesWholeField(t *testing.T) {
	got, err := overlayFor(t, baseSpec(), "-e", "MODE=dev", "-e", "DEBUG=1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Env) != 2 || got.Env["MODE"] != "dev" || got.Env["DEBUG"] != "1" {
		t.Errorf("env = %v, want fully replaced {MODE:dev DEBUG:1}", got.Env)
	}
}

func TestOverlayCompositeGroupsKeepUnsetSubfields(t *testing.T) {
	// Network: changing the CIDR list keeps the hostname allowlist.
	got, err := overlayFor(t, baseSpec(), "--net-allow-cidr", "203.0.113.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if got.Network == nil || len(got.Network.Allowlist) != 1 || got.Network.Allowlist[0] != "pypi.org" ||
		len(got.Network.AllowlistCIDR) != 1 {
		t.Errorf("network = %+v, want allowlist kept + cidr added", got.Network)
	}
	// Sleep: min-scale alone keeps the other sleep subfields.
	base := baseSpec()
	base.Sleep = &api.SleepPolicy{MinScale: 0, IdleTimeoutSec: 300, KeepConnections: true}
	got, err = overlayFor(t, base, "--min-scale", "2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sleep.MinScale != 2 || got.Sleep.IdleTimeoutSec != 300 || !got.Sleep.KeepConnections {
		t.Errorf("sleep = %+v, want min_scale 2 with idle/keep kept", got.Sleep)
	}
}

func TestOverlayClearsHealthAndRedirect(t *testing.T) {
	got, err := overlayFor(t, baseSpec(), "--health", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Health != nil {
		t.Errorf("health = %+v, want cleared", got.Health)
	}
	off := false
	base := baseSpec()
	base.HTTPRedirect = &off
	got, err = overlayFor(t, base, "--no-https-redirect=false")
	if err != nil {
		t.Fatal(err)
	}
	if got.HTTPRedirect != nil {
		t.Errorf("http_redirect = %v, want nil (back to default)", *got.HTTPRedirect)
	}
}

func TestOverlayHealthFlagsMutuallyExclusive(t *testing.T) {
	if _, err := overlayFor(t, baseSpec(), "--health", "tcp:80", "--health-cmd", "true"); err == nil {
		t.Error("want error for --health with --health-cmd")
	}
}

func TestOverlayNoFlagsIsIdentity(t *testing.T) {
	got, err := overlayFor(t, baseSpec())
	if err != nil {
		t.Fatal(err)
	}
	want := baseSpec()
	if got.MemoryMiB != want.MemoryMiB || got.Env["MODE"] != "prod" ||
		got.Sleep == nil || got.Sleep.MinScale != 1 || got.Restart.Policy != wire.RestartAlways {
		t.Errorf("no-flag overlay changed the spec: %+v", got)
	}
}
