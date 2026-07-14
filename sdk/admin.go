package crucible

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AdminBackup streams a daemon backup (tar.gz of the daemon's app,
// token, volume-record, and registry-credential stores plus a manifest) to w.
// Requires the `admin_backup` scoped-token op — the archive carries usable
// registry secrets. Volume DATA is not included; pair with volume backups.
func (c *Client) AdminBackup(ctx context.Context, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/admin/backup", nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect to daemon at %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("backup failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
