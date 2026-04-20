package fcapi

import "context"

// BootSource describes the guest kernel Firecracker will boot.
//
// Firecracker has no firmware — it jumps straight to the kernel entry
// point. KernelImagePath points to an uncompressed vmlinux-style image.
// BootArgs is the kernel command line; common choices for our use case:
//
//		"console=ttyS0 reboot=k panic=1 pci=off"
//
//	  - console=ttyS0   wires the guest console to the VMM process stdout.
//	  - reboot=k        on `reboot`, invoke the keyboard controller, which
//	                    Firecracker catches and uses to exit cleanly.
//	  - panic=1         panic immediately on kernel panic (don't hang).
//	  - pci=off         Firecracker doesn't expose a PCI bus; don't probe.
//
// InitrdPath is optional; leave empty for direct rootfs boots.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// PutBootSource configures the guest kernel. Must be called before
// InstanceStart; can be replaced any time before the VM is started.
func (c *Client) PutBootSource(ctx context.Context, src BootSource) error {
	return c.do(ctx, "PUT", "/boot-source", src, nil)
}

// MachineConfig controls CPU and memory allocation for the guest.
// Fields match the Firecracker API verbatim; add more (smt, cpu_template,
// track_dirty_pages, huge_pages) when a feature needs them.
type MachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

// PutMachineConfig sets the machine configuration. Must be called before
// InstanceStart.
func (c *Client) PutMachineConfig(ctx context.Context, cfg MachineConfig) error {
	return c.do(ctx, "PUT", "/machine-config", cfg, nil)
}
