package fcapi

import "context"

// VmState enumerates the transitional states Firecracker accepts on
// PATCH /vm. Values travel on the wire verbatim.
type VmState string

const (
	// VmStatePaused stops the VM's vCPU threads without destroying the
	// instance — a prerequisite for taking a snapshot.
	VmStatePaused VmState = "Paused"

	// VmStateResumed returns a Paused VM to running state. No-op on an
	// already-running VM (Firecracker returns 204 either way).
	VmStateResumed VmState = "Resumed"
)

type vmStateReq struct {
	State VmState `json:"state"`
}

// PutVmState transitions the VM to the requested state. Pause before
// taking a snapshot (Firecracker rejects /snapshot/create otherwise).
// Resume when the snapshot is saved and you want the source sandbox to
// keep running.
func (c *Client) PutVmState(ctx context.Context, state VmState) error {
	return c.do(ctx, "PATCH", "/vm", vmStateReq{State: state}, nil)
}
