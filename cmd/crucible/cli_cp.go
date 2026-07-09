package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newCpCmd implements `crucible cp` — docker/kubectl-style file transfer between
// the host and a sandbox. A `sbx_...:<path>` operand is the sandbox side; the
// other is a local path. v0.3.2 ships the push direction (host -> guest); the
// pull direction lands alongside it.
func newCpCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "cp <src> <dst>",
		Short: "Copy files or directories into a sandbox",
		Long: "Copy a local file or directory into a running sandbox:\n" +
			"    crucible cp ./app sbx_abc123:/work\n\n" +
			"Directories are copied recursively. The destination is treated as a " +
			"directory in the guest; the source's basename is preserved under it " +
			"(so `cp ./app sbx:/work` lands at `/work/app`). Parent directories are " +
			"created as needed and existing files are overwritten.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcID, srcPath, srcRemote := parseCpArg(args[0])
			dstID, dstPath, dstRemote := parseCpArg(args[1])
			switch {
			case !srcRemote && dstRemote:
				return runCpPush(cmd, o, srcPath, dstID, dstPath)
			case srcRemote && !dstRemote:
				_ = srcID
				return errors.New("copying out of a sandbox (sbx:path -> local) is not supported yet")
			case srcRemote && dstRemote:
				return errors.New("copying between two sandboxes is not supported")
			default:
				return errors.New("exactly one operand must be a sandbox path, e.g. sbx_abc123:/work")
			}
		},
	}
}

// parseCpArg splits a `sbx_<id>:<path>` operand into its parts. An operand is a
// sandbox reference when it starts with the sandbox id prefix and contains a
// colon; anything else is a local path.
func parseCpArg(s string) (id, path string, remote bool) {
	const prefix = "sbx_"
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		for i := len(prefix); i < len(s); i++ {
			if s[i] == ':' {
				return s[:i], s[i+1:], true
			}
		}
	}
	return "", s, false
}

// runCpPush tars the local path and streams it into the sandbox.
func runCpPush(cmd *cobra.Command, o *globalOpts, local, id, dest string) error {
	if _, err := os.Stat(local); err != nil {
		return err
	}
	if dest == "" {
		dest = "/"
	}

	// Build the tar in a goroutine and stream it as the request body so a large
	// project never lives in memory whole.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarLocalPath(local, pw)) }()

	res, err := o.client().CopyTo(cmd.Context(), id, dest, pr)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"copied %s -> %s:%s (%d file(s), %d bytes)\n", local, id, dest, res.Files, res.Bytes)
	return nil
}

// tarLocalPath writes a tar of local (a file or directory) to w. Entry names
// are relative to local's parent, so the basename is preserved (a later extract
// beneath a destination dir reproduces the tree under `<dest>/<basename>`).
func tarLocalPath(local string, w io.Writer) error {
	tw := tar.NewWriter(w)
	base := filepath.Dir(local)
	walkErr := filepath.WalkDir(local, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var link string
		if d.Type()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.Type().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	return tw.Close()
}
