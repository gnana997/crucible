package fcapi

import "context"

// ActionType enumerates the instance actions Firecracker accepts on
// PUT /actions. We surface only the ones crucible uses.
type ActionType string

const (
	// ActionInstanceStart powers on a configured-but-not-yet-running VM.
	// All of boot-source, drives, and machine-config must already be set.
	ActionInstanceStart ActionType = "InstanceStart"

	// ActionSendCtrlAltDel asks the guest to shut down gracefully via an
	// ACPI power event. The guest must have an ACPI-aware shutdown handler
	// (systemd does). For hard shutdown, kill the firecracker process.
	ActionSendCtrlAltDel ActionType = "SendCtrlAltDel"
)

type action struct {
	ActionType ActionType `json:"action_type"`
}

// InstanceStart boots the VM. The API returns 204 immediately once the
// guest VCPU threads are spawned; the guest kernel then proceeds on its
// own. To observe boot progress, attach to the serial console or poll
// GetInstanceInfo.
func (c *Client) InstanceStart(ctx context.Context) error {
	return c.do(ctx, "PUT", "/actions", action{ActionType: ActionInstanceStart}, nil)
}

// SendCtrlAltDel sends the guest an ACPI shutdown request. The guest is
// expected to run its shutdown sequence and exit; Firecracker itself
// stays alive until the guest halts. If the guest ignores the signal,
// the caller should fall back to killing the firecracker process.
func (c *Client) SendCtrlAltDel(ctx context.Context) error {
	return c.do(ctx, "PUT", "/actions", action{ActionType: ActionSendCtrlAltDel}, nil)
}
