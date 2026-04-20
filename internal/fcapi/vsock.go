package fcapi

import "context"

// VsockConfig describes the virtio-vsock device attached to the guest.
//
// Firecracker exposes a single per-VM vsock device. On the host side it
// materializes as the unix socket at UDSPath — Firecracker creates it
// during PUT /vsock. Host processes use Firecracker's "hybrid vsock"
// protocol on that socket to open streams to guest-side AF_VSOCK
// listeners: write "CONNECT <port>\n", read "OK <fd>\n", then stream.
//
// GuestCID is the CID assigned to the guest endpoint. The host is
// always CID 2; crucible uses 3 for every guest (CID namespaces are
// isolated per VM, so two sandboxes using CID 3 don't collide).
type VsockConfig struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// PutVsock attaches (or reconfigures) the guest vsock device. Must be
// called before InstanceStart — Firecracker rejects late changes.
func (c *Client) PutVsock(ctx context.Context, cfg VsockConfig) error {
	return c.do(ctx, "PUT", "/vsock", cfg, nil)
}
