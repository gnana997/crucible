package sandbox

import (
	"context"
	"fmt"

	"github.com/gnana997/crucible/internal/agentapi"
	"github.com/gnana997/crucible/internal/agentwire"
)

// serviceClient resolves a sandbox's agent client for service calls,
// mirroring Exec's guards: unknown id → ErrNotFound; a sandbox without
// a vsock channel can't supervise anything.
func (m *Manager) serviceClient(sandboxID string) (*agentapi.Client, error) {
	s, err := m.Get(sandboxID)
	if err != nil {
		return nil, err
	}
	if s.execClient == nil {
		return nil, fmt.Errorf("sandbox %s has no agent channel (no vsock)", sandboxID)
	}
	return s.execClient, nil
}

// ConfigureService installs or replaces the sandbox's supervised
// service spec (see agentwire.ServiceSpec).
func (m *Manager) ConfigureService(ctx context.Context, sandboxID string, spec *agentwire.ServiceSpec) (agentwire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return agentwire.ServiceStatus{}, err
	}
	return c.ConfigureService(ctx, spec)
}

// StartService launches the sandbox's configured service.
func (m *Manager) StartService(ctx context.Context, sandboxID string) (agentwire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return agentwire.ServiceStatus{}, err
	}
	return c.StartService(ctx)
}

// StopService stops the sandbox's service. graceSec > 0 overrides the
// spec's stop grace for this stop.
func (m *Manager) StopService(ctx context.Context, sandboxID string, graceSec int) (agentwire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return agentwire.ServiceStatus{}, err
	}
	return c.StopService(ctx, graceSec)
}

// RestartService stops (if running) and relaunches the service.
func (m *Manager) RestartService(ctx context.Context, sandboxID string) (agentwire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return agentwire.ServiceStatus{}, err
	}
	return c.RestartService(ctx)
}

// ServiceStatus reports the supervisor state for the sandbox.
func (m *Manager) ServiceStatus(ctx context.Context, sandboxID string) (agentwire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return agentwire.ServiceStatus{}, err
	}
	return c.ServiceStatus(ctx)
}

// ServiceLogs reads captured service output from the sandbox's log
// ring, starting at fromSeq.
func (m *Manager) ServiceLogs(ctx context.Context, sandboxID string, fromSeq uint64, maxBytes int) (agentwire.ServiceLogsResponse, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return agentwire.ServiceLogsResponse{}, err
	}
	return c.ServiceLogs(ctx, fromSeq, maxBytes)
}
