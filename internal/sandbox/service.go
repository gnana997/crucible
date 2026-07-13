package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/gnana997/crucible/internal/agentapi"
	"github.com/gnana997/crucible/sdk/wire"
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
// service spec (see wire.ServiceSpec).
func (m *Manager) ConfigureService(ctx context.Context, sandboxID string, spec *wire.ServiceSpec) (wire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return c.ConfigureService(ctx, spec)
}

// Quiesce flushes the sandbox guest's filesystems (agent sync) so a
// subsequent stop doesn't lose un-fsync'd writes to a Writeback volume.
// Best-effort: a sandbox with no agent channel (or gone) is a no-op, and an
// old agent without /quiesce is tolerated.
func (m *Manager) Quiesce(ctx context.Context, sandboxID string) error {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return nil // no agent channel or already gone — nothing to flush
	}
	if err := c.Quiesce(ctx); err != nil && !errors.Is(err, agentapi.ErrQuiesceUnsupported) {
		return err
	}
	return nil
}

// StartService launches the sandbox's configured service.
func (m *Manager) StartService(ctx context.Context, sandboxID string) (wire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return c.StartService(ctx)
}

// StopService stops the sandbox's service. graceSec > 0 overrides the
// spec's stop grace for this stop.
func (m *Manager) StopService(ctx context.Context, sandboxID string, graceSec int) (wire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return c.StopService(ctx, graceSec)
}

// RestartService stops (if running) and relaunches the service.
func (m *Manager) RestartService(ctx context.Context, sandboxID string) (wire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return c.RestartService(ctx)
}

// ServiceStatus reports the supervisor state for the sandbox.
func (m *Manager) ServiceStatus(ctx context.Context, sandboxID string) (wire.ServiceStatus, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return wire.ServiceStatus{}, err
	}
	return c.ServiceStatus(ctx)
}

// ServiceLogs reads captured service output from the sandbox's log
// ring, starting at fromSeq.
func (m *Manager) ServiceLogs(ctx context.Context, sandboxID string, fromSeq uint64, maxBytes int) (wire.ServiceLogsResponse, error) {
	c, err := m.serviceClient(sandboxID)
	if err != nil {
		return wire.ServiceLogsResponse{}, err
	}
	return c.ServiceLogs(ctx, fromSeq, maxBytes)
}
