package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gnana997/crucible/sdk/wire"
)

// ErrServiceUnsupported reports that the guest agent predates the
// /service endpoints (HTTP 404) — the rootfs was built with an older
// crucible-agent. Fix: rebuild the rootfs with the current agent.
var ErrServiceUnsupported = errors.New(
	"agentapi: guest agent does not support service supervision — rebuild the rootfs with the current crucible-agent")

// ErrNoServiceConfigured mirrors the agent's 409: a start/restart/logs
// call arrived before any service spec was configured.
var ErrNoServiceConfigured = errors.New("agentapi: no service configured")

// ConfigureService installs (or replaces) the supervised service spec.
// If the service is running it is stopped under the old spec's grace
// and relaunched under the new one.
func (c *Client) ConfigureService(ctx context.Context, spec *wire.ServiceSpec) (wire.ServiceStatus, error) {
	return c.serviceCall(ctx, http.MethodPut, "http://agent/service", spec)
}

// StartService launches the configured service. Idempotent: starting a
// running service is a no-op success.
func (c *Client) StartService(ctx context.Context) (wire.ServiceStatus, error) {
	return c.serviceCall(ctx, http.MethodPost, "http://agent/service/start", nil)
}

// StopService stops the service (StopSignal → grace → SIGKILL).
// graceSec > 0 overrides the spec's grace for this stop. The call
// returns as soon as the stop is initiated; poll ServiceStatus for the
// terminal state.
func (c *Client) StopService(ctx context.Context, graceSec int) (wire.ServiceStatus, error) {
	var body any
	if graceSec > 0 {
		body = wire.ServiceStopRequest{GraceSec: graceSec}
	}
	return c.serviceCall(ctx, http.MethodPost, "http://agent/service/stop", body)
}

// RestartService stops (if running) and relaunches the service.
func (c *Client) RestartService(ctx context.Context) (wire.ServiceStatus, error) {
	return c.serviceCall(ctx, http.MethodPost, "http://agent/service/restart", nil)
}

// ServiceStatus reports the supervisor's current state.
func (c *Client) ServiceStatus(ctx context.Context) (wire.ServiceStatus, error) {
	return c.serviceCall(ctx, http.MethodGet, "http://agent/service/status", nil)
}

// ServiceLogs reads captured service output from the agent's ring
// buffer, starting at fromSeq. maxBytes <= 0 uses the agent default.
func (c *Client) ServiceLogs(ctx context.Context, fromSeq uint64, maxBytes int) (wire.ServiceLogsResponse, error) {
	url := "http://agent/service/logs?from_seq=" + strconv.FormatUint(fromSeq, 10)
	if maxBytes > 0 {
		url += "&max_bytes=" + strconv.Itoa(maxBytes)
	}
	var out wire.ServiceLogsResponse
	err := c.serviceDo(ctx, http.MethodGet, url, nil, &out)
	return out, err
}

func (c *Client) serviceCall(ctx context.Context, method, url string, body any) (wire.ServiceStatus, error) {
	var out wire.ServiceStatus
	err := c.serviceDo(ctx, method, url, body, &out)
	return out, err
}

// serviceDo performs one JSON request/response against the agent's
// service API, mapping the agent's error statuses onto typed errors.
func (c *Client) serviceDo(ctx context.Context, method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentapi: service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRefreshBody))
		return ErrServiceUnsupported
	case resp.StatusCode == http.StatusConflict:
		return ErrNoServiceConfigured
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
		return fmt.Errorf("agentapi: service returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	// Success bodies are small JSON; logs responses are bounded by the
	// agent's own max_bytes cap plus base64 overhead. Cap generously so
	// a compromised agent can't stream unbounded data.
	limited := io.LimitReader(resp.Body, 8<<20)
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("agentapi: decode service response: %w", err)
	}
	return nil
}
