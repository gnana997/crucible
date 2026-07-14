package crucible

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gnana997/crucible/sdk/api"
)

// CreateVolume provisions a persistent volume (POST /volumes). A zero SizeBytes
// uses the daemon default. Errors 409 if the name already exists.
func (c *Client) CreateVolume(ctx context.Context, req api.CreateVolumeRequest) (api.Volume, error) {
	resp, err := c.do(ctx, http.MethodPost, "/volumes", req)
	if err != nil {
		return api.Volume{}, err
	}
	return decodeInto[api.Volume](resp)
}

// ListVolumes returns every volume with its live attachment (GET /volumes).
func (c *Client) ListVolumes(ctx context.Context) (Page[api.Volume], error) {
	resp, err := c.do(ctx, http.MethodGet, "/volumes", nil)
	if err != nil {
		return Page[api.Volume]{}, err
	}
	out, err := decodeInto[api.VolumeListResponse](resp)
	return Page[api.Volume]{Items: out.Volumes}, err
}

// GetVolume fetches one volume by name (GET /volumes/{name}).
func (c *Client) GetVolume(ctx context.Context, name string) (api.Volume, error) {
	resp, err := c.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return api.Volume{}, err
	}
	return decodeInto[api.Volume](resp)
}

// DeleteVolume removes a volume and its backing file (DELETE /volumes/{name}).
// Errors 409 if the volume is attached to a live sandbox.
func (c *Client) DeleteVolume(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// BackupVolume takes a point-in-time backup of a volume
// (POST /volumes/{name}/backups). Errors 409 if the volume is attached to a
// running sandbox (sleep it first).
func (c *Client) BackupVolume(ctx context.Context, name string) (api.Backup, error) {
	resp, err := c.do(ctx, http.MethodPost, "/volumes/"+url.PathEscape(name)+"/backups", nil)
	if err != nil {
		return api.Backup{}, err
	}
	return decodeInto[api.Backup](resp)
}

// ListBackups returns volume backups (GET /backups, or GET
// /volumes/{name}/backups when volumeName is non-empty).
func (c *Client) ListBackups(ctx context.Context, volumeName string) (Page[api.Backup], error) {
	path := "/backups"
	if volumeName != "" {
		path = "/volumes/" + url.PathEscape(volumeName) + "/backups"
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Page[api.Backup]{}, err
	}
	out, err := decodeInto[api.BackupListResponse](resp)
	return Page[api.Backup]{Items: out.Backups}, err
}

// ExportOptions configures a backup export. Raw streams the backing file
// uncompressed; the default (Raw false) gzips it, which collapses the sparse
// ext4 image's holes over the wire.
type ExportOptions struct {
	Raw bool
}

// ExportBackup streams a backup's bytes off the host to w (GET
// /backups/{id}/export), so a caller (the control plane) can ship it to an
// object store. Requires the `volume_backup` scoped-token op — it moves volume
// data across the boundary. Returns the decompressed byte size (the
// X-Crucible-Backup-Size header), which the caller can verify against what it
// stores. Gzip by default; pass ExportOptions{Raw: true} for the raw stream.
func (c *Client) ExportBackup(ctx context.Context, id string, opt ExportOptions, w io.Writer) (int64, error) {
	u := c.base + "/backups/" + url.PathEscape(id) + "/export"
	if opt.Raw {
		u += "?compress=none"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("connect to daemon at %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("export failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	size, _ := strconv.ParseInt(resp.Header.Get("X-Crucible-Backup-Size"), 10, 64)
	if _, err := io.Copy(w, resp.Body); err != nil {
		return size, err
	}
	return size, nil
}

// ImportOptions describes an incoming backup stream: the volume it came from
// (recorded for the catalog), its consistency level (default "filesystem"), and
// whether the stream is gzip-compressed (as ExportBackup produces by default).
type ImportOptions struct {
	SourceVolume string
	Consistency  string
	Raw          bool // the stream is uncompressed (default: gzip)
}

// ImportBackup streams a backup's bytes onto the host (POST /backups/import) and
// returns the new record; RestoreBackup then materialises a volume from it. This
// is how the control plane restores an off-host backup onto a (possibly fresh)
// host. Requires the `volume_backup` scoped-token op.
func (c *Client) ImportBackup(ctx context.Context, opt ImportOptions, r io.Reader) (api.Backup, error) {
	q := url.Values{}
	q.Set("source", opt.SourceVolume)
	if opt.Consistency != "" {
		q.Set("consistency", opt.Consistency)
	}
	if opt.Raw {
		q.Set("compress", "none")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/backups/import?"+q.Encode(), r)
	if err != nil {
		return api.Backup{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return api.Backup{}, fmt.Errorf("connect to daemon at %s: %w", c.base, err)
	}
	return decodeInto[api.Backup](resp) // handles status >= 400 + closes the body
}

// DeleteBackup removes a backup and its backing file (DELETE /backups/{id}).
func (c *Client) DeleteBackup(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/backups/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return expectNoContent(resp)
}

// RestoreBackup materialises a backup into a new volume newName
// (POST /volumes/{newName}/restore). Errors 409 if newName already exists, 404
// if the backup is gone.
func (c *Client) RestoreBackup(ctx context.Context, backupID, newName string) (api.Volume, error) {
	resp, err := c.do(ctx, http.MethodPost, "/volumes/"+url.PathEscape(newName)+"/restore", api.RestoreVolumeRequest{From: backupID})
	if err != nil {
		return api.Volume{}, err
	}
	return decodeInto[api.Volume](resp)
}

// CloneVolume copies a quiescent volume src into a new volume dst
// (POST /volumes/{src}/clone). Errors 409 if dst exists or src is attached to a
// running sandbox.
func (c *Client) CloneVolume(ctx context.Context, src, dst string) (api.Volume, error) {
	resp, err := c.do(ctx, http.MethodPost, "/volumes/"+url.PathEscape(src)+"/clone", api.CloneVolumeRequest{To: dst})
	if err != nil {
		return api.Volume{}, err
	}
	return decodeInto[api.Volume](resp)
}
