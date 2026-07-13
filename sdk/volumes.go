package crucible

import (
	"context"
	"net/http"
	"net/url"

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
