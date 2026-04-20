package fcapi

import "context"

// InstanceState is the VM lifecycle state reported by Firecracker. These
// values come directly from the API — we don't translate them.
type InstanceState string

const (
	StateNotStarted InstanceState = "Not started"
	StateRunning    InstanceState = "Running"
	StatePaused     InstanceState = "Paused"
)

// InstanceInfo is the JSON shape returned by GET /.
type InstanceInfo struct {
	ID         string        `json:"id"`
	State      InstanceState `json:"state"`
	VMMVersion string        `json:"vmm_version"`
	AppName    string        `json:"app_name"`
}

// GetInstanceInfo returns the VM's current state. Useful as a readiness
// probe: right after firecracker is spawned, this returns "Not started"
// once the API socket is ready to accept requests.
func (c *Client) GetInstanceInfo(ctx context.Context) (InstanceInfo, error) {
	var info InstanceInfo
	if err := c.do(ctx, "GET", "/", nil, &info); err != nil {
		return InstanceInfo{}, err
	}
	return info, nil
}
