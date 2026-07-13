package crucible

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// CaptureOptions filter and bound a packet capture. Zero fields take the
// daemon's defaults (whole-packet snaplen, 50 MiB, 60 s).
type CaptureOptions struct {
	Filter     string // BPF expression (e.g. "tcp port 80"); empty = all
	Snaplen    int    // bytes captured per packet
	MaxBytes   int64  // hard cap on streamed bytes
	MaxSeconds int    // hard cap on duration
}

// Capture streams a live pcap of a sandbox's traffic to w until a cap is hit,
// ctx is cancelled, or the daemon ends it. Requires the `capture` scoped-token
// op (payloads are sensitive). Write w to a file or pipe it to Wireshark.
func (c *Client) Capture(ctx context.Context, sandboxID string, opt CaptureOptions, w io.Writer) error {
	q := url.Values{}
	if opt.Filter != "" {
		q.Set("filter", opt.Filter)
	}
	if opt.Snaplen > 0 {
		q.Set("snaplen", strconv.Itoa(opt.Snaplen))
	}
	if opt.MaxBytes > 0 {
		q.Set("max_bytes", strconv.FormatInt(opt.MaxBytes, 10))
	}
	if opt.MaxSeconds > 0 {
		q.Set("max_seconds", strconv.Itoa(opt.MaxSeconds))
	}
	u := c.base + "/sandboxes/" + url.PathEscape(sandboxID) + "/capture"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
		return fmt.Errorf("capture failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(w, resp.Body)
	if ctx.Err() != nil {
		return nil // user cancel / deadline is a clean stop
	}
	return err
}
