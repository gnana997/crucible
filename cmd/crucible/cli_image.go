package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newImageCmd(o *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "image",
		Short:   "Manage the OCI image cache (experimental)",
		Long:    "Pull or side-load OCI/Docker images and convert them into bootable crucible rootfs artifacts. Booting sandboxes from images lands in a later release; this manages the cache.",
		Aliases: []string{"img"},
	}
	cmd.AddCommand(
		newImagePullCmd(o),
		newImageImportCmd(o),
		newImageLsCmd(o),
		newImageInspectCmd(o),
		newImageRmCmd(o),
	)
	return cmd
}

func newImagePullCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "pull <ref>",
		Short: "Pull and convert an image (e.g. nginx:latest, ghcr.io/org/app:v1)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			img, err := o.client().PullImage(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), img)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), img.Digest)
			return nil
		},
	}
}

func newImageImportCmd(o *globalOpts) *cobra.Command {
	var (
		file string
		tag  string
	)
	cmd := &cobra.Command{
		Use:   "import [--file <archive.tar>] [--tag <repo:tag>]",
		Short: "Side-load a docker-save archive (reads stdin when --file is omitted)",
		Long:  "Import an image from a `docker save` tar archive. Pipe it on stdin (docker save img | crucible image import) or pass --file. --tag selects one image from a multi-image archive.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := cmd.InOrStdin()
			if file != "" {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				in = f
			}
			img, err := o.client().ImportImage(cmd.Context(), in, tag)
			if err != nil {
				return err
			}
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), img)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), img.Digest)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "docker-save archive path (default: stdin)")
	cmd.Flags().StringVar(&tag, "tag", "", "select this image from a multi-image archive")
	return cmd
}

func newImageLsCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Short:   "List converted images",
		Aliases: []string{"list"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			imgsPage, err := o.client().ListImages(cmd.Context())
			if err != nil {
				return err
			}
			imgs := imgsPage.Items
			if o.isJSON() {
				return printJSON(cmd.OutOrStdout(), imgs)
			}
			tw := newTable(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(tw, "DIGEST\tREF\tSIZE(MiB)\tMODE\tENTRYPOINT")
			for _, img := range imgs {
				ep := "-"
				if len(img.Entrypoint) > 0 {
					ep = img.Entrypoint[0]
				} else if len(img.Cmd) > 0 {
					ep = img.Cmd[0]
				}
				ref := img.SourceRef
				if ref == "" {
					ref = "-"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
					shortDigest(img.Digest), ref, img.SizeBytes>>20, img.ConvertMode, ep)
			}
			return tw.Flush()
		},
	}
}

func newImageInspectCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <ref>",
		Short: "Show an image's full details (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			img, err := o.client().GetImage(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), img)
		},
	}
}

func newImageRmCmd(o *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <ref>...",
		Short:   "Delete one or more images (by digest, hex prefix, or ref)",
		Aliases: []string{"delete"},
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := o.client()
			for _, ref := range args {
				if err := cl.DeleteImage(cmd.Context(), ref); err != nil {
					return fmt.Errorf("delete %s: %w", ref, err)
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), ref)
			}
			return nil
		},
	}
}

// shortDigest trims "sha256:<hex>" to a docker-style 12-char id.
func shortDigest(d string) string {
	hex := d
	if _, after, ok := strings.Cut(d, ":"); ok {
		hex = after
	}
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}
