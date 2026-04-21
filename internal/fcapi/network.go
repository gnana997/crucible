package fcapi

import (
	"context"
	"fmt"
)

// NetworkInterface is the body of PUT /network-interfaces/{iface_id}.
// Configures the guest's virtio-net backing: Firecracker attaches to
// the named host TAP device and presents it to the guest under the
// given MAC.
//
// Fields we don't populate in v0.1 (rx_rate_limiter, tx_rate_limiter,
// allow_mmds_requests) are deliberately absent — Firecracker defaults
// are correct for our use case.
type NetworkInterface struct {
	// IfaceID is the string used in the URL path. Must match the
	// guest's kernel view of the interface (which for a single-NIC
	// setup is "eth0"). Firecracker uses this as the device's
	// stable identifier across snapshot/restore.
	IfaceID string `json:"iface_id"`

	// HostDevName is the host-side TAP interface Firecracker
	// attaches to. Relative to the process's network namespace —
	// under jailer this is the tap inside the sandbox's netns.
	HostDevName string `json:"host_dev_name"`

	// GuestMAC is the MAC address the guest sees on eth0. Should
	// be locally-administered (first byte's bit 1 set) to avoid
	// collisions with real-world OUIs.
	GuestMAC string `json:"guest_mac,omitempty"`
}

// PutNetworkInterface issues PUT /network-interfaces/{iface_id}.
// Pre-boot only — this is a "boot-specific resource" in Firecracker
// terms; calling after InstanceStart returns 400.
func (c *Client) PutNetworkInterface(ctx context.Context, cfg NetworkInterface) error {
	if cfg.IfaceID == "" {
		return fmt.Errorf("fcapi: PutNetworkInterface: IfaceID required")
	}
	return c.do(ctx, "PUT", "/network-interfaces/"+cfg.IfaceID, cfg, nil)
}
