package client

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gnana997/crucible/sdk/wire"
)

// ConfigureService installs or replaces a sandbox's supervised service
// spec (PUT /sandboxes/{id}/service). If the service is running it is
// restarted under the new spec.
func (c *Client) ConfigureService(ctx context.Context, sandboxID string, spec wire.ServiceSpec) (wire.ServiceStatus, error) {
	resp, err := c.do(ctx, http.MethodPut, "/sandboxes/"+sandboxID+"/service", spec)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return decodeInto[wire.ServiceStatus](resp)
}

// StartService launches the configured service (POST .../service/start).
// Idempotent: starting a running service is a no-op success.
func (c *Client) StartService(ctx context.Context, sandboxID string) (wire.ServiceStatus, error) {
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/service/start", nil)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return decodeInto[wire.ServiceStatus](resp)
}

// StopService stops the service (POST .../service/stop). graceSec > 0
// overrides the spec's stop grace. Returns as soon as the stop is
// initiated; poll ServiceStatus for the terminal state.
func (c *Client) StopService(ctx context.Context, sandboxID string, graceSec int) (wire.ServiceStatus, error) {
	var body any
	if graceSec > 0 {
		body = wire.ServiceStopRequest{GraceSec: graceSec}
	}
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/service/stop", body)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return decodeInto[wire.ServiceStatus](resp)
}

// RestartService stops (if running) and relaunches the service
// (POST .../service/restart).
func (c *Client) RestartService(ctx context.Context, sandboxID string) (wire.ServiceStatus, error) {
	resp, err := c.do(ctx, http.MethodPost, "/sandboxes/"+sandboxID+"/service/restart", nil)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return decodeInto[wire.ServiceStatus](resp)
}

// ServiceStatus reports the supervisor's state (GET .../service).
func (c *Client) ServiceStatus(ctx context.Context, sandboxID string) (wire.ServiceStatus, error) {
	resp, err := c.do(ctx, http.MethodGet, "/sandboxes/"+sandboxID+"/service", nil)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return decodeInto[wire.ServiceStatus](resp)
}

// ServiceLogs reads captured service output from the agent's ring
// buffer (GET .../service/logs), starting at fromSeq. maxBytes <= 0
// uses the server default.
func (c *Client) ServiceLogs(ctx context.Context, sandboxID string, fromSeq uint64, maxBytes int) (wire.ServiceLogsResponse, error) {
	path := "/sandboxes/" + sandboxID + "/service/logs?from_seq=" + strconv.FormatUint(fromSeq, 10)
	if maxBytes > 0 {
		path += "&max_bytes=" + strconv.Itoa(maxBytes)
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return wire.ServiceLogsResponse{}, err
	}
	return decodeInto[wire.ServiceLogsResponse](resp)
}
