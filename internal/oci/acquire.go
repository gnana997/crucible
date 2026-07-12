package oci

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// targetPlatform is the only platform crucible can boot.
var targetPlatform = v1.Platform{OS: "linux", Architecture: "amd64"}

// Acquired is an image that passed acquisition: resolved to
// linux/amd64, platform-validated, with its runtime contract
// extracted. It is the input to the assembler.
type Acquired struct {
	// Image is the resolved platform image (never an index).
	Image v1.Image

	// SourceRef is the fully-qualified pull reference, or
	// "docker-archive" (plus the tag when one was given) for
	// side-loads.
	SourceRef string

	// Digest is the platform image's manifest digest ("sha256:…") —
	// the store's content-address key.
	Digest string

	// RunConfig is the extracted runtime contract (run.json v1,
	// conversion stamps not yet filled — the assembler adds those).
	RunConfig *RunConfig
}

// PullOption customizes Pull.
type PullOption func(*pullConfig)

type pullConfig struct {
	nameOpts []name.Option
	keychain authn.Keychain
	auth     authn.Authenticator
}

// PullAuth is a one-shot credential for a single pull (the per-request
// `--registry-auth` path): the registry is implied by the ref being pulled.
type PullAuth struct {
	Username string
	Secret   string
}

// WithInsecureRegistry allows plain-HTTP registries. Used by tests
// against in-process registries; also the knob for LAN registries.
func WithInsecureRegistry() PullOption {
	return func(c *pullConfig) { c.nameOpts = append(c.nameOpts, name.Insecure) }
}

// WithAuth pins a single authenticator for this pull, taking precedence over any
// keychain — the per-request private-registry path.
func WithAuth(a authn.Authenticator) PullOption {
	return func(c *pullConfig) { c.auth = a }
}

// WithKeychain resolves per-registry credentials at pull time (the private-
// registry path). The keychain is consulted for the target registry host; a
// host it doesn't recognize falls back to anonymous, so public pulls are
// unaffected. Without this option a pull stays anonymous, so a host's docker
// login can't leak into daemon behavior.
func WithKeychain(kc authn.Keychain) PullOption {
	return func(c *pullConfig) { c.keychain = kc }
}

// Pull fetches an image reference and resolves it to linux/amd64. Multi-arch
// indexes resolve to the matching child (a missing child is an error); a
// single-platform image of another arch/OS is rejected. Auth is anonymous
// unless WithKeychain supplies per-registry credentials.
func Pull(ctx context.Context, ref string, opts ...PullOption) (*Acquired, error) {
	var cfg pullConfig
	for _, o := range opts {
		o(&cfg)
	}
	parsed, err := name.ParseReference(ref, cfg.nameOpts...)
	if err != nil {
		return nil, fmt.Errorf("oci: parse reference %q: %w", ref, err)
	}

	// Precedence: an explicit per-request authenticator > the store keychain >
	// anonymous. So a one-shot --registry-auth overrides a stored credential.
	authOpt := remote.WithAuth(authn.Anonymous)
	switch {
	case cfg.auth != nil:
		authOpt = remote.WithAuth(cfg.auth)
	case cfg.keychain != nil:
		authOpt = remote.WithAuthFromKeychain(cfg.keychain)
	}
	img, err := remote.Image(parsed,
		remote.WithContext(ctx),
		authOpt,
		remote.WithPlatform(targetPlatform),
	)
	if err != nil {
		return nil, fmt.Errorf("oci: pull %s: %w", parsed.Name(), err)
	}
	return finishAcquire(img, parsed.Name())
}

// ImportDockerArchive side-loads a docker-save tarball from disk. tag
// selects the image when the archive holds several ("repo:tag" as
// docker save wrote it); empty tag requires a single-image archive.
func ImportDockerArchive(path, tag string) (*Acquired, error) {
	var tagPtr *name.Tag
	sourceRef := "docker-archive"
	if tag != "" {
		t, err := name.NewTag(tag)
		if err != nil {
			return nil, fmt.Errorf("oci: parse archive tag %q: %w", tag, err)
		}
		tagPtr = &t
		sourceRef = "docker-archive:" + t.Name()
	}
	img, err := tarball.ImageFromPath(path, tagPtr)
	if err != nil {
		return nil, fmt.Errorf("oci: import docker archive %s: %w", path, err)
	}
	return finishAcquire(img, sourceRef)
}

// finishAcquire runs the shared tail of both acquisition paths:
// platform validation, digest resolution, contract extraction.
func finishAcquire(img v1.Image, sourceRef string) (*Acquired, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("oci: read image config: %w", err)
	}
	if err := validatePlatform(cf); err != nil {
		return nil, err
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("oci: compute image digest: %w", err)
	}
	return &Acquired{
		Image:     img,
		SourceRef: sourceRef,
		Digest:    digest.String(),
		RunConfig: extractRunConfig(cf, sourceRef, digest.String()),
	}, nil
}
