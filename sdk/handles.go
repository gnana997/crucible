package crucible

import (
	"context"
	"io"

	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// Handles are chaining sugar over the flat Client methods: cheap value
// structs holding the client and an ID, no extra round-trips. Use them
// when a piece of code operates on one sandbox/snapshot repeatedly —
//
//	sbx := cr.Sandbox(id)
//	res, err := sbx.Exec(ctx, req, os.Stdout, os.Stderr)
//	snap, err := sbx.Snapshot(ctx)
//	forks, err := snap.Fork(ctx, 8)
//
// — and use the flat methods when you want the full response DTOs (the
// handle-returning methods trade the response body for chainability).

// Sandbox returns a handle for one sandbox. Purely local: the ID is not
// checked against the daemon until a method call.
func (c *Client) Sandbox(id string) Sandbox {
	return Sandbox{ID: id, c: c}
}

// SnapshotHandle returns a handle for one snapshot. Purely local.
// (Named to leave room for the flat Snapshot method, which captures one.)
func (c *Client) SnapshotHandle(id string) Snapshot {
	return Snapshot{ID: id, c: c}
}

// Sandbox is a handle on one sandbox.
type Sandbox struct {
	ID string
	c  *Client
}

// Get fetches the sandbox's current state.
func (s Sandbox) Get(ctx context.Context) (api.SandboxResponse, error) {
	return s.c.GetSandbox(ctx, s.ID)
}

// Delete destroys the sandbox.
func (s Sandbox) Delete(ctx context.Context) error {
	return s.c.DeleteSandbox(ctx, s.ID)
}

// Exec runs a command and streams output; see Client.Exec.
func (s Sandbox) Exec(ctx context.Context, req wire.ExecRequest, stdout, stderr io.Writer) (wire.ExecResult, error) {
	return s.c.Exec(ctx, s.ID, req, stdout, stderr)
}

// ExecInteractive opens a full-duplex session; see Client.ExecInteractive.
func (s Sandbox) ExecInteractive(ctx context.Context, req wire.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (wire.ExecResult, error) {
	return s.c.ExecInteractive(ctx, s.ID, req, stdin, stdout, stderr)
}

// CopyTo streams a tar archive into the sandbox; see Client.CopyTo.
func (s Sandbox) CopyTo(ctx context.Context, dest string, tar io.Reader) (wire.FilesPutResult, error) {
	return s.c.CopyTo(ctx, s.ID, dest, tar)
}

// ReadFile reads one guest file; see Client.ReadFile.
func (s Sandbox) ReadFile(ctx context.Context, path string, maxBytes int) ([]byte, error) {
	return s.c.ReadFile(ctx, s.ID, path, maxBytes)
}

// Logs reads the sandbox's durable logs; see Client.Logs.
func (s Sandbox) Logs(ctx context.Context, since int64, source string) (api.LogsResponse, error) {
	return s.c.Logs(ctx, s.ID, since, source)
}

// Snapshot captures the sandbox and returns a handle on the new
// snapshot. Use the flat Client.Snapshot when you need the full
// SnapshotResponse instead of a handle.
func (s Sandbox) Snapshot(ctx context.Context) (Snapshot, error) {
	resp, err := s.c.Snapshot(ctx, s.ID)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{ID: resp.ID, c: s.c}, nil
}

// ConfigureService installs or replaces the supervised service spec.
func (s Sandbox) ConfigureService(ctx context.Context, spec wire.ServiceSpec) (wire.ServiceStatus, error) {
	return s.c.ConfigureService(ctx, s.ID, spec)
}

// StartService launches the configured service.
func (s Sandbox) StartService(ctx context.Context) (wire.ServiceStatus, error) {
	return s.c.StartService(ctx, s.ID)
}

// StopService stops the service; graceSec > 0 overrides the spec's grace.
func (s Sandbox) StopService(ctx context.Context, graceSec int) (wire.ServiceStatus, error) {
	return s.c.StopService(ctx, s.ID, graceSec)
}

// RestartService stops (if running) and relaunches the service.
func (s Sandbox) RestartService(ctx context.Context) (wire.ServiceStatus, error) {
	return s.c.RestartService(ctx, s.ID)
}

// ServiceStatus reports the supervisor's state.
func (s Sandbox) ServiceStatus(ctx context.Context) (wire.ServiceStatus, error) {
	return s.c.ServiceStatus(ctx, s.ID)
}

// ServiceLogs reads captured service output from the agent's ring buffer.
func (s Sandbox) ServiceLogs(ctx context.Context, fromSeq uint64, maxBytes int) (wire.ServiceLogsResponse, error) {
	return s.c.ServiceLogs(ctx, s.ID, fromSeq, maxBytes)
}

// Snapshot is a handle on one snapshot.
type Snapshot struct {
	ID string
	c  *Client
}

// Get fetches the snapshot's current state.
func (sn Snapshot) Get(ctx context.Context) (api.SnapshotResponse, error) {
	return sn.c.GetSnapshot(ctx, sn.ID)
}

// Delete removes the snapshot.
func (sn Snapshot) Delete(ctx context.Context) error {
	return sn.c.DeleteSnapshot(ctx, sn.ID)
}

// Fork creates count sandboxes from the snapshot and returns handles on
// them. Optional publish mappings expose the fork's ports on the host
// (count must be 1). Use the flat Client.Fork when you need the full
// SandboxResponses (IP addresses etc.) instead of handles.
func (sn Snapshot) Fork(ctx context.Context, count int, publish ...api.PortMapping) ([]Sandbox, error) {
	resps, err := sn.c.Fork(ctx, sn.ID, count, publish...)
	if err != nil {
		return nil, err
	}
	out := make([]Sandbox, len(resps))
	for i, r := range resps {
		out[i] = Sandbox{ID: r.ID, c: sn.c}
	}
	return out, nil
}
