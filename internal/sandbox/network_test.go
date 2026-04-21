package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubAllowlist satisfies sandbox.NetworkAllowlist for unit tests.
// Never invoked — just needs to be non-nil.
type stubAllowlist struct{}

func (stubAllowlist) Matches(string) bool   { return true }
func (stubAllowlist) Patterns() []string    { return nil }

// stubProvisioner records Setup/Teardown calls without doing
// anything real. Used to exercise the sandbox-layer branches
// independent of the internal/network host machinery.
type stubProvisioner struct {
	setups    int
	teardowns int
	setupErr  error
}

func (s *stubProvisioner) Setup(context.Context, NetworkSetupRequest) (*NetworkHandle, error) {
	s.setups++
	if s.setupErr != nil {
		return nil, s.setupErr
	}
	return &NetworkHandle{
		NetnsPath: "/var/run/netns/crucible-stub",
		TapName:   "tap0",
		GuestMAC:  "02:00:00:00:00:01",
		GuestIP:   "10.20.0.3",
		Gateway:   "10.20.0.1",
		Allowlist: stubAllowlist{},
	}, nil
}

func (s *stubProvisioner) Teardown(context.Context, *NetworkHandle) error {
	s.teardowns++
	return nil
}

func TestProvisionNetworkNilIsNoop(t *testing.T) {
	m := &Manager{}
	h, err := m.provisionNetwork(context.Background(), "sbx", nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if h != nil {
		t.Errorf("handle should be nil for nil request, got %+v", h)
	}
}

func TestProvisionNetworkRejectsWhenProvisionerNil(t *testing.T) {
	m := &Manager{cfg: ManagerConfig{}} // Network: nil
	_, err := m.provisionNetwork(context.Background(), "sbx", &NetworkConfig{
		Allowlist: stubAllowlist{},
	})
	if err == nil {
		t.Fatal("expected error when network requested but provisioner missing")
	}
	if !strings.Contains(err.Error(), "no network provisioner") {
		t.Errorf("err = %v, want mention of missing provisioner", err)
	}
}

func TestProvisionNetworkRequiresAllowlist(t *testing.T) {
	m := &Manager{cfg: ManagerConfig{Network: &stubProvisioner{}}}
	_, err := m.provisionNetwork(context.Background(), "sbx", &NetworkConfig{})
	if err == nil {
		t.Fatal("expected error for nil Allowlist")
	}
	if !strings.Contains(err.Error(), "Allowlist required") {
		t.Errorf("err = %v, want 'Allowlist required'", err)
	}
}

func TestProvisionNetworkSanitizesIDForNetNS(t *testing.T) {
	// Sandbox IDs contain underscores (sbx_abc...) but netns
	// names can't. The provisioner must see the sanitized form.
	p := &stubProvisioner{}
	m := &Manager{cfg: ManagerConfig{Network: p}}

	var seen NetworkSetupRequest
	p2 := &recordingProvisioner{inner: p, record: &seen}
	m.cfg.Network = p2

	_, err := m.provisionNetwork(context.Background(), "sbx_abc123", &NetworkConfig{
		Allowlist: stubAllowlist{},
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if seen.SandboxID != "sbx-abc123" {
		t.Errorf("provisioner saw SandboxID=%q, want %q (underscore sanitized)",
			seen.SandboxID, "sbx-abc123")
	}
}

func TestProvisionNetworkWrapsUnderlyingError(t *testing.T) {
	p := &stubProvisioner{setupErr: errors.New("netlink: EPERM")}
	m := &Manager{cfg: ManagerConfig{Network: p}}
	_, err := m.provisionNetwork(context.Background(), "sbx", &NetworkConfig{
		Allowlist: stubAllowlist{},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "EPERM") {
		t.Errorf("underlying error should propagate, got %v", err)
	}
	if !strings.Contains(err.Error(), "network setup") {
		t.Errorf("error should be wrapped with 'network setup', got %v", err)
	}
}

func TestSanitizeNetworkID(t *testing.T) {
	cases := map[string]string{
		"sbx_abc":        "sbx-abc",
		"snap_l87p5q":    "snap-l87p5q",
		"already-clean":  "already-clean",
		"":               "",
		"_leading":       "-leading",
		"trailing_":      "trailing-",
		"a_b_c_d":        "a-b-c-d",
	}
	for in, want := range cases {
		if got := sanitizeNetworkID(in); got != want {
			t.Errorf("sanitizeNetworkID(%q) = %q, want %q", in, got, want)
		}
	}
}

// recordingProvisioner wraps another provisioner and captures the
// SetupRequest it saw. Used to assert sandbox.Manager passes the
// right arguments through.
type recordingProvisioner struct {
	inner  NetworkProvisioner
	record *NetworkSetupRequest
}

func (r *recordingProvisioner) Setup(ctx context.Context, req NetworkSetupRequest) (*NetworkHandle, error) {
	*r.record = req
	return r.inner.Setup(ctx, req)
}

func (r *recordingProvisioner) Teardown(ctx context.Context, h *NetworkHandle) error {
	return r.inner.Teardown(ctx, h)
}
