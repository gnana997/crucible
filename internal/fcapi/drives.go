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
