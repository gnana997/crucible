package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	client "github.com/gnana997/crucible/sdk"
)

// resolveCreateImage decides which image reference the daemon should
// boot from, mirroring `docker run` precedence. A locally-built Docker
// image the (Docker-free) daemon can't see is saved and imported here,
// client-side, and referenced by its converted digest; everything else
// is passed through unchanged for the daemon to resolve from its store
// or a registry under the pull policy. The local-Docker step is
// skipped when --pull always (force a fresh registry pull) or Docker is
// unavailable — Docker is a convenience here, never a requirement.
//
// Returns the ref to send and the pull policy to send with it (empty for
// a just-imported digest, which is already in the store).
func resolveCreateImage(ctx context.Context, cl *client.Client, image, pull string, progress io.Writer) (string, string, error) {
	if pull == "always" || !dockerImagePresent(ctx, image) {
		return image, pull, nil
	}
	_, _ = fmt.Fprintf(progress, "found local docker image %q; importing into crucible…\n", image)
	digest, err := sideloadDockerImage(ctx, cl, image)
	if err != nil {
		return "", "", fmt.Errorf("import local docker image %q: %w", image, err)
	}
	return digest, "", nil
}

// dockerImagePresent reports whether the local Docker daemon has an
// image by this name. Absent Docker (or any error) means "no", so the
// caller falls through to a daemon-side store/registry resolve.
func dockerImagePresent(ctx context.Context, ref string) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	// `docker image inspect` exits non-zero for an unknown image.
	return exec.CommandContext(ctx, "docker", "image", "inspect", ref).Run() == nil
}

// sideloadDockerImage streams `docker save <ref>` into the daemon's
// image import endpoint and returns the converted image's digest.
func sideloadDockerImage(ctx context.Context, cl *client.Client, ref string) (string, error) {
	// A local cancel lets us unblock a docker-save that is mid-write if
	// the import fails early (otherwise save.Wait would hang on the pipe).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	save := exec.CommandContext(ctx, "docker", "save", ref)
	stdout, err := save.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	save.Stderr = &stderr
	if err := save.Start(); err != nil {
		return "", err
	}

	img, importErr := cl.ImportImage(ctx, stdout, "")
	if importErr != nil {
		cancel() // stop a docker save still writing into the closed reader
	}
	waitErr := save.Wait()

	if importErr != nil {
		return "", importErr
	}
	if waitErr != nil {
		return "", fmt.Errorf("docker save %q: %w: %s", ref, waitErr, strings.TrimSpace(stderr.String()))
	}
	return img.Digest, nil
}
