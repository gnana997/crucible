package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// newBuildCmd builds a Dockerfile locally and loads the result into
// crucible's image store in one verb: `docker build` client-side, then the
// same docker-save → import path `sandbox create --image <local-tag>` uses.
// Docker stays a client-side convenience — the daemon never needs it.
func newBuildCmd(o *globalOpts) *cobra.Command {
	var (
		tag        string
		dockerfile string
	)
	cmd := &cobra.Command{
		Use:   "build [flags] <context>",
		Short: "Build a Dockerfile and load the image into crucible's store",
		Long: "Run `docker build` on a build context locally, then import the result " +
			"into crucible's image store and print the converted image digest — ready " +
			"for `crucible run <digest>` or `sandbox create --image <digest>`.\n\n" +
			"Docker is used client-side only (the daemon stays Docker-free). With -t " +
			"the built image is also left tagged in your local Docker.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := exec.LookPath("docker"); err != nil {
				return fmt.Errorf("crucible build needs the `docker` CLI on PATH (it shells out to `docker build`): %w", err)
			}

			// `docker build -q` prints only the resulting image ID on stdout
			// (progress goes to stderr), giving us a stable reference to save —
			// with or without -t.
			build := exec.CommandContext(cmd.Context(), "docker", dockerBuildArgs(tag, dockerfile, args[0])...)
			var idOut strings.Builder
			build.Stdout = &idOut
			build.Stderr = cmd.ErrOrStderr()
			if err := build.Run(); err != nil {
				return fmt.Errorf("docker build: %w", err)
			}
			ref := strings.TrimSpace(idOut.String())
			if ref == "" {
				return fmt.Errorf("docker build produced no image id")
			}

			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "importing built image into crucible…\n")
			digest, err := sideloadDockerImage(cmd.Context(), o.client(), ref)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), digest)
			return nil
		},
	}
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "also tag the built image in local Docker (docker build -t)")
	cmd.Flags().StringVarP(&dockerfile, "file", "f", "", "path to the Dockerfile (docker build -f)")
	return cmd
}

// dockerBuildArgs assembles the `docker build` argv. `-q` makes stdout the
// image ID; -t / -f are forwarded when set. Pure so it's unit-testable
// without invoking docker.
func dockerBuildArgs(tag, dockerfile, contextDir string) []string {
	args := []string{"build", "-q"}
	if tag != "" {
		args = append(args, "-t", tag)
	}
	if dockerfile != "" {
		args = append(args, "-f", dockerfile)
	}
	return append(args, contextDir)
}
