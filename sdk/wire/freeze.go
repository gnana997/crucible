package wire

// FreezeSpec instructs the guest agent to freeze (FIFREEZE) or thaw (FITHAW) one
// mounted filesystem so the host can take a filesystem-consistent copy of its
// backing file while the guest keeps running. The daemon sends it over vsock
// (POST /freeze then POST /thaw) around a live volume backup, freezing only the
// volume's mount — never the rootfs the agent itself runs from, so the agent
// stays responsive to the paired /thaw.
type FreezeSpec struct {
	// Mountpoint is the absolute guest path of the filesystem to freeze/thaw,
	// e.g. "/var/lib/postgresql/data" (the volume's mount, not "/").
	Mountpoint string `json:"mountpoint"`
}
