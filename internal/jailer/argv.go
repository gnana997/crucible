package jailer

import (
	"fmt"
	"strconv"
)

// BuildArgs returns the argv to pass to jailer (excluding argv[0],
// the jailer binary path — the caller prepends that when building an
// exec.Cmd).
//
// fcArgs is forwarded to firecracker after the "--" separator. Jailer
// always injects --id and timing args into firecracker's own argv, so
// callers typically leave fcArgs empty and configure firecracker
// entirely over the API socket after boot.
//
// Shape of the returned slice:
//
//	--id <ID>
//	--exec-file <ExecFile>
//	--uid <UID> --gid <GID>
//	--chroot-base-dir <ChrootBase>
//	--cgroup-version 2
//	[--cgroup cpu.max=<Quotas.CPUMax>]
//	[--cgroup memory.max=<Quotas.MemoryMaxBytes>]
//	[--cgroup pids.max=<Quotas.PIDsMax>]
//	[--new-pid-ns]
//	[-- <fcArgs...>]
//
// BuildArgs does not re-validate the Spec — call Spec.Validate first
// if the Spec came from user input. BuildArgs is a pure function with
// no IO.
func BuildArgs(spec Spec, fcArgs []string) []string {
	args := []string{
		"--id", spec.ID,
		"--exec-file", spec.ExecFile,
		"--uid", strconv.FormatUint(uint64(spec.UID), 10),
		"--gid", strconv.FormatUint(uint64(spec.GID), 10),
		"--chroot-base-dir", spec.ChrootBase,
		"--cgroup-version", "2",
	}

	// Emit only the quotas the caller actually set. A zero-valued
	// field means "no limit" — omit the flag entirely so jailer
	// doesn't write a conflicting cgroup entry.
	if spec.Quotas.CPUMax != "" {
		args = append(args, "--cgroup", "cpu.max="+spec.Quotas.CPUMax)
	}
	if spec.Quotas.MemoryMaxBytes > 0 {
		args = append(args, "--cgroup", fmt.Sprintf("memory.max=%d", spec.Quotas.MemoryMaxBytes))
	}
	if spec.Quotas.PIDsMax > 0 {
		args = append(args, "--cgroup", fmt.Sprintf("pids.max=%d", spec.Quotas.PIDsMax))
	}

	if spec.NewPIDNS {
		args = append(args, "--new-pid-ns")
	}

	if spec.NetNSPath != "" {
		args = append(args, "--netns", spec.NetNSPath)
	}

	if len(fcArgs) > 0 {
		args = append(args, "--")
		args = append(args, fcArgs...)
	}

	return args
}
