package fcapi

import (
	"context"
	"net/url"
)

// Drive describes a block device attached to the guest.
//
// For crucible v0.1, every sandbox has exactly one drive with
// IsRootDevice=true, pointing at a pre-baked rootfs.ext4. Adding more
// drives (for writable scratch volumes or snapshots) is a v0.2 concern.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// PutDrive attaches or replaces a block device identified by d.DriveID.
// The drive_id in the URL path and the JSON body must match; PutDrive
// enforces this by deriving the URL from d.DriveID directly.
func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	path := "/drives/" + url.PathEscape(d.DriveID)
	return c.do(ctx, "PUT", path, d, nil)
}

// DrivePatch is the minimal body for PATCH /drives/{id}. Firecracker
// accepts a subset of Drive fields here — path_on_host is the one we
// care about for snapshot+fork (swapping the backing file after load
// so each fork writes to its own rootfs copy). Other rotateable fields
// (rate_limiter) can be added when we need them.
type DrivePatch struct {
	DriveID    string `json:"drive_id"`
	PathOnHost string `json:"path_on_host,omitempty"`
}

// PatchDrive hot-swaps the backing file for an already-attached drive.
// Safe to call on a Paused VM between snapshot/load and resume — that's
// how forks get their own writable rootfs without sharing the source's.
func (c *Client) PatchDrive(ctx context.Context, p DrivePatch) error {
	path := "/drives/" + url.PathEscape(p.DriveID)
	return c.do(ctx, "PATCH", path, p, nil)
}
