package daemon

import (
	"context"
	"fmt"

	"github.com/gnana997/crucible/internal/network"
	"github.com/gnana997/crucible/internal/sandbox"
)

// networkAdapter wraps *network.Manager to satisfy the narrow
// sandbox.NetworkProvisioner interface.
//
// Lives in the daemon package (not sandbox) because sandbox must
// not depend on internal/network — the adapter is the single
// translation point between the two packages' type surfaces.
type networkAdapter struct {
	m *network.Manager
}

// NewNetworkAdapter wraps a *network.Manager for use as the
// NetworkProvisioner on sandbox.ManagerConfig.
func NewNetworkAdapter(m *network.Manager) sandbox.NetworkProvisioner {
	return &networkAdapter{m: m}
}

// Setup translates the sandbox-layer request into the network-
// layer Setup call and packages the returned handle into the
// sandbox-layer view.
func (a *networkAdapter) Setup(ctx context.Context, req sandbox.NetworkSetupRequest) (*sandbox.NetworkHandle, error) {
	// The allowlist the sandbox package hands us is our own
	// network.Allowlist (the daemon parses into it and stores
	// on the sandbox-level NetworkConfig). Assert back to the
	// concrete type — if anyone ever hands us a different
	// Matcher implementation through this path, fail loudly;
	// it's a wiring bug.
	al, ok := req.Allowlist.(*network.Allowlist)
	if !ok {
		return nil, fmt.Errorf("daemon: network adapter expected *network.Allowlist, got %T", req.Allowlist)
	}
	inner, err := a.m.Setup(ctx, network.SandboxSetup{
		SandboxID: req.SandboxID,
		Allowlist: al,
	})
	if err != nil {
		return nil, err
	}

	return &sandbox.NetworkHandle{
		NetnsPath: inner.NetnsPath,
		TapName:   inner.TapName,
		GuestMAC:  formatMAC(inner.GuestMAC),
		GuestIP:   inner.Lease.GuestIP.String(),
		Gateway:   inner.Lease.Gateway.String(),
		Allowlist: al,
		Impl:      inner,
	}, nil
}

// Teardown reverses Setup. Expects the Impl field to carry the
// original *network.SandboxHandle; if it doesn't, the handle
// was fabricated by a test that bypassed the adapter and we
// silently no-op.
func (a *networkAdapter) Teardown(ctx context.Context, h *sandbox.NetworkHandle) error {
	if h == nil {
		return nil
	}
	inner, ok := h.Impl.(*network.SandboxHandle)
	if !ok {
		return nil
	}
	return a.m.Teardown(ctx, inner)
}

// formatMAC turns [6]byte into the colon-separated hex string
// Firecracker's PutNetworkInterface expects. Done here so the
// sandbox package stays agnostic of Firecracker's wire format.
func formatMAC(mac [6]byte) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
}
