package jailer

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ChrootRoot returns the absolute host path to this spec's chroot
// root — the directory jailer pivot_root's into before exec'ing
// firecracker. Layout matches jailer's own convention:
// <ChrootBase>/<basename(ExecFile)>/<ID>/root/.
//
// Example: ChrootBase=/srv/jailer, ExecFile=/usr/bin/firecracker,
// ID=sbx_abc → /srv/jailer/firecracker/sbx_abc/root.
func ChrootRoot(spec Spec) string {
	return filepath.Join(spec.ChrootBase, filepath.Base(spec.ExecFile), spec.ID, "root")
}

// ChrootDir returns the parent of ChrootRoot — the directory jailer
// created to hold this VM's chroot. Used by Cleanup to remove the
// whole VM slot in one os.RemoveAll.
func ChrootDir(spec Spec) string {
	return filepath.Join(spec.ChrootBase, filepath.Base(spec.ExecFile), spec.ID)
}

// HostPath translates a chroot-relative path (as firecracker sees it
// after pivot_root) into its absolute location on the host, for
// daemon-side file operations.
//
// Leading slashes on chrootRel are normalized away — "/v.sock",
// "v.sock", and "///v.sock" all resolve identically.
func HostPath(spec Spec, chrootRel string) string {
	cleaned := filepath.Clean("/" + strings.TrimPrefix(chrootRel, "/"))
	return filepath.Join(ChrootRoot(spec), strings.TrimPrefix(cleaned, "/"))
}

// ChrootRel is the inverse of HostPath: given an absolute host path
// inside this spec's chroot, return the chroot-relative path that
// firecracker should be passed over the API.
//
// Returns an error if hostAbs is not under ChrootRoot(spec) — that's
// a caller bug, not a recoverable situation, so we surface it
// rather than silently returning a ".." path.
func ChrootRel(spec Spec, hostAbs string) (string, error) {
	root := ChrootRoot(spec)
	rel, err := filepath.Rel(root, hostAbs)
	if err != nil {
		return "", fmt.Errorf("jailer: compute rel path %s under %s: %w", hostAbs, root, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("jailer: %s is outside chroot %s", hostAbs, root)
	}
	if rel == "." {
		return "/", nil
	}
	return "/" + rel, nil
}
