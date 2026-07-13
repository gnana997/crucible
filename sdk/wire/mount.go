package wire

// MountSpec instructs the guest agent to mount a persistent volume's block
// device at a path inside the guest. The daemon sends it over vsock
// (POST /mount) after the agent is healthy and before the workload starts,
// so a volume-backed app (e.g. postgres) sees its data directory mounted
// before it launches. Mounting is idempotent — a wake-path re-mount is safe.
type MountSpec struct {
	// Device is the guest block device, e.g. "/dev/vdb". The rootfs is
	// /dev/vda; the first attached volume is /dev/vdb, the next /dev/vdc.
	Device string `json:"device"`

	// Mountpoint is an absolute path in the guest, e.g.
	// "/var/lib/postgresql/data". Created if absent.
	Mountpoint string `json:"mountpoint"`

	// Fstype is the filesystem, e.g. "ext4". Empty defaults to "ext4".
	Fstype string `json:"fstype,omitempty"`
}
