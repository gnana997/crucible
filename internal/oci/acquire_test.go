package oci

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// newFakeRegistry starts an in-process registry and returns its
// host:port. Tests talk plain HTTP via WithInsecureRegistry — no
// network leaves the process.
func newFakeRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// craftImage builds a small single-layer image with realistic file modes and
// rewrites its config to the given platform + runtime config.
//
// We hand-build the layer rather than use random.Image: the latter emits
// mode-0000 files, which an unprivileged `mkfs.ext4 -d <dir>` (staging-mode
// conversion) cannot read — it fails with "Permission denied while
// populating". The daemon runs as root, so production is unaffected, but the
// unprivileged test environment (CI) hits it whenever the host lacks tarball
// (pipe-mode) mkfs support. A layer with normal perms converts everywhere.
func craftImage(t *testing.T, os, arch string, cfg v1.Config) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(h *tar.Header, body []byte) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("tar header %s: %v", h.Name, err)
		}
		if len(body) > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("tar body %s: %v", h.Name, err)
			}
		}
	}
	write(&tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	body := []byte("crucible-test-payload")
	write(&tar.Header{Name: "app/data.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}, body)
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	if err != nil {
		t.Fatalf("LayerFromOpener: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	cf = cf.DeepCopy()
	cf.OS = os
	cf.Architecture = arch
	cf.Config = cfg
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}
	return img
}

func pushImage(t *testing.T, ref string, img v1.Image) {
	t.Helper()
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}
	if err := remote.Write(parsed, img); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
}

// newAuthedRegistry wraps the in-process registry with HTTP Basic auth, so a
// pull must present the credential (the private-registry path).
func newAuthedRegistry(t *testing.T, user, pass string) string {
	t.Helper()
	inner := registry.New()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="crucible-test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// basicKeychain is a fixed authn.Keychain supplying one credential for any host.
type basicKeychain struct{ user, pass string }

func (k basicKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return authn.FromConfig(authn.AuthConfig{Username: k.user, Password: k.pass}), nil
}

func pushImageAuth(t *testing.T, ref string, img v1.Image, kc authn.Keychain) {
	t.Helper()
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}
	if err := remote.Write(parsed, img, remote.WithAuthFromKeychain(kc)); err != nil {
		t.Fatalf("push %s (auth): %v", ref, err)
	}
}

// TestPullWithKeychainBasicAuth: WithKeychain authenticates a private pull; the
// same pull is refused anonymously or with a wrong credential.
func TestPullWithKeychainBasicAuth(t *testing.T) {
	const user, pass = "alice", "s3cret-token"
	reg := newAuthedRegistry(t, user, pass)
	ref := reg + "/team/private:latest"
	img := craftImage(t, "linux", "amd64", v1.Config{})
	good := basicKeychain{user, pass}

	pushImageAuth(t, ref, img, good)

	// Right credential → pull succeeds and resolves the pushed digest.
	got, err := Pull(t.Context(), ref, WithInsecureRegistry(), WithKeychain(good))
	if err != nil {
		t.Fatalf("authenticated pull failed: %v", err)
	}
	if got.Digest != mustDigest(t, img) {
		t.Errorf("digest = %s, want %s", got.Digest, mustDigest(t, img))
	}

	// Anonymous (no keychain) → auth error.
	if _, err := Pull(t.Context(), ref, WithInsecureRegistry()); err == nil {
		t.Error("anonymous pull of a private image succeeded; want an auth error")
	}

	// Wrong secret → auth error.
	if _, err := Pull(t.Context(), ref, WithInsecureRegistry(), WithKeychain(basicKeychain{user, "wrong"})); err == nil {
		t.Error("pull with a wrong credential succeeded; want an auth error")
	}
}

// TestPullPerRequestAuthOverridesKeychain: a one-shot WithAuth wins over a
// (wrong) store keychain — the per-request --registry-auth precedence.
func TestPullPerRequestAuthOverridesKeychain(t *testing.T) {
	const user, pass = "alice", "s3cret-token"
	reg := newAuthedRegistry(t, user, pass)
	ref := reg + "/team/private:latest"
	img := craftImage(t, "linux", "amd64", v1.Config{})
	pushImageAuth(t, ref, img, basicKeychain{user, pass})

	got, err := Pull(t.Context(), ref,
		WithInsecureRegistry(),
		WithKeychain(basicKeychain{user, "wrong-stored-secret"}),                     // store cred is wrong
		WithAuth(authn.FromConfig(authn.AuthConfig{Username: user, Password: pass}))) // per-request wins
	if err != nil {
		t.Fatalf("per-request auth did not override the keychain: %v", err)
	}
	if got.Digest != mustDigest(t, img) {
		t.Errorf("digest = %s, want %s", got.Digest, mustDigest(t, img))
	}
}

func mustDigest(t *testing.T, img v1.Image) string {
	t.Helper()
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return d.String()
}

func TestPullSingleImageAndExtract(t *testing.T) {
	reg := newFakeRegistry(t)
	img := craftImage(t, "linux", "amd64", v1.Config{
		Entrypoint:   []string{"/app/serve"},
		Cmd:          []string{"--port", "8080"},
		Env:          []string{"PATH=/custom/bin", "APP_MODE=prod"},
		WorkingDir:   "/app",
		User:         "1000:1000",
		StopSignal:   "SIGINT",
		ExposedPorts: map[string]struct{}{"9090/tcp": {}, "8080/tcp": {}},
		Volumes:      map[string]struct{}{"/data": {}},
		Labels:       map[string]string{"maintainer": "smoke"},
		Healthcheck: &v1.HealthConfig{
			Test:        []string{"CMD", "/app/healthz"},
			Interval:    30 * time.Second,
			Timeout:     5 * time.Second,
			StartPeriod: 10 * time.Second,
			Retries:     3,
		},
	})
	ref := reg + "/apps/web:v1"
	pushImage(t, ref, img)

	got, err := Pull(t.Context(), ref, WithInsecureRegistry())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got.Digest != mustDigest(t, img) {
		t.Errorf("digest = %s, want %s", got.Digest, mustDigest(t, img))
	}
	if !strings.HasSuffix(got.SourceRef, "/apps/web:v1") {
		t.Errorf("SourceRef = %q, want fully-qualified ref", got.SourceRef)
	}

	rc := got.RunConfig
	if rc.Version != RunConfigVersion {
		t.Errorf("Version = %d, want %d", rc.Version, RunConfigVersion)
	}
	if len(rc.Entrypoint) != 1 || rc.Entrypoint[0] != "/app/serve" {
		t.Errorf("Entrypoint = %v", rc.Entrypoint)
	}
	if len(rc.Cmd) != 2 || rc.Cmd[1] != "8080" {
		t.Errorf("Cmd = %v", rc.Cmd)
	}
	if len(rc.Env) != 2 || rc.Env[0] != "PATH=/custom/bin" || rc.Env[1] != "APP_MODE=prod" {
		t.Errorf("Env order not preserved: %v", rc.Env)
	}
	if rc.WorkingDir != "/app" || rc.User != "1000:1000" || rc.StopSignal != "SIGINT" {
		t.Errorf("wd/user/stop = %q %q %q", rc.WorkingDir, rc.User, rc.StopSignal)
	}
	if len(rc.ExposedPorts) != 2 || rc.ExposedPorts[0] != "8080/tcp" || rc.ExposedPorts[1] != "9090/tcp" {
		t.Errorf("ExposedPorts not sorted: %v", rc.ExposedPorts)
	}
	if len(rc.Volumes) != 1 || rc.Volumes[0] != "/data" {
		t.Errorf("Volumes = %v", rc.Volumes)
	}
	if rc.Labels["maintainer"] != "smoke" {
		t.Errorf("Labels = %v", rc.Labels)
	}
	hc := rc.Healthcheck
	if hc == nil || hc.Test[0] != "CMD" || hc.IntervalMs != 30_000 || hc.TimeoutMs != 5_000 ||
		hc.StartPeriodMs != 10_000 || hc.Retries != 3 {
		t.Errorf("Healthcheck = %+v", hc)
	}
	if rc.Digest != got.Digest || rc.SourceRef != got.SourceRef {
		t.Errorf("RunConfig stamps: digest %q ref %q", rc.Digest, rc.SourceRef)
	}
}

func TestPullResolvesMultiArchIndexToAmd64(t *testing.T) {
	reg := newFakeRegistry(t)
	amd := craftImage(t, "linux", "amd64", v1.Config{Cmd: []string{"/bin/app"}})
	arm := craftImage(t, "linux", "arm64", v1.Config{Cmd: []string{"/bin/app"}})

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add:        amd,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
		},
		mutate.IndexAddendum{
			Add:        arm,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		},
	)
	ref := reg + "/apps/multi:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := remote.WriteIndex(parsed, idx); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	got, err := Pull(t.Context(), ref, WithInsecureRegistry())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got.Digest != mustDigest(t, amd) {
		t.Errorf("resolved digest = %s, want the amd64 child %s", got.Digest, mustDigest(t, amd))
	}
}

func TestPullRejectsIndexWithoutAmd64(t *testing.T) {
	reg := newFakeRegistry(t)
	arm := craftImage(t, "linux", "arm64", v1.Config{})
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add:        arm,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		},
	)
	ref := reg + "/apps/armonly:latest"
	parsed, _ := name.ParseReference(ref, name.Insecure)
	if err := remote.WriteIndex(parsed, idx); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	if _, err := Pull(t.Context(), ref, WithInsecureRegistry()); err == nil {
		t.Fatal("Pull of an index without linux/amd64 succeeded")
	}
}

func TestPullRejectsWrongPlatformImage(t *testing.T) {
	reg := newFakeRegistry(t)
	for _, tc := range []struct{ os, arch string }{
		{"linux", "arm64"},
		{"windows", "amd64"},
	} {
		img := craftImage(t, tc.os, tc.arch, v1.Config{})
		ref := reg + "/apps/" + tc.os + tc.arch + ":latest"
		pushImage(t, ref, img)
		_, err := Pull(t.Context(), ref, WithInsecureRegistry())
		if err == nil || !strings.Contains(err.Error(), "requires linux/amd64") {
			t.Errorf("%s/%s: err = %v, want linux/amd64 rejection", tc.os, tc.arch, err)
		}
	}
}

func TestPullBadReference(t *testing.T) {
	if _, err := Pull(t.Context(), "not a valid ref!!"); err == nil {
		t.Fatal("bad reference accepted")
	}
}

func TestPullUnknownRepo(t *testing.T) {
	reg := newFakeRegistry(t)
	if _, err := Pull(t.Context(), reg+"/nope/missing:latest", WithInsecureRegistry()); err == nil {
		t.Fatal("pull of a missing repo succeeded")
	}
}

func TestImportDockerArchiveSingle(t *testing.T) {
	img := craftImage(t, "linux", "amd64", v1.Config{
		Cmd: []string{"/bin/app"},
		Env: []string{"FROM_ARCHIVE=1"},
	})
	tag, err := name.NewTag("example.com/side/load:v2")
	if err != nil {
		t.Fatalf("NewTag: %v", err)
	}
	path := filepath.Join(t.TempDir(), "img.tar")
	if err := tarball.WriteToFile(path, tag, img); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	got, err := ImportDockerArchive(path, "")
	if err != nil {
		t.Fatalf("ImportDockerArchive: %v", err)
	}
	if got.Digest != mustDigest(t, img) {
		t.Errorf("digest = %s, want %s", got.Digest, mustDigest(t, img))
	}
	if got.SourceRef != "docker-archive" {
		t.Errorf("SourceRef = %q", got.SourceRef)
	}
	if len(got.RunConfig.Env) != 1 || got.RunConfig.Env[0] != "FROM_ARCHIVE=1" {
		t.Errorf("RunConfig.Env = %v", got.RunConfig.Env)
	}
}

func TestImportDockerArchiveMultiNeedsTag(t *testing.T) {
	imgA := craftImage(t, "linux", "amd64", v1.Config{Labels: map[string]string{"which": "a"}})
	imgB := craftImage(t, "linux", "amd64", v1.Config{Labels: map[string]string{"which": "b"}})
	tagA, _ := name.NewTag("example.com/multi/a:latest")
	tagB, _ := name.NewTag("example.com/multi/b:latest")

	path := filepath.Join(t.TempDir(), "multi.tar")
	if err := tarball.MultiRefWriteToFile(path, map[name.Reference]v1.Image{tagA: imgA, tagB: imgB}); err != nil {
		t.Fatalf("MultiRefWriteToFile: %v", err)
	}

	if _, err := ImportDockerArchive(path, ""); err == nil {
		t.Fatal("multi-image archive without a tag accepted")
	}
	got, err := ImportDockerArchive(path, "example.com/multi/b:latest")
	if err != nil {
		t.Fatalf("ImportDockerArchive with tag: %v", err)
	}
	if got.RunConfig.Labels["which"] != "b" {
		t.Errorf("selected wrong image: labels = %v", got.RunConfig.Labels)
	}
	if !strings.Contains(got.SourceRef, "multi/b") {
		t.Errorf("SourceRef = %q, want the tag recorded", got.SourceRef)
	}
}

func TestImportDockerArchiveRejectsWrongArch(t *testing.T) {
	img := craftImage(t, "linux", "arm64", v1.Config{})
	tag, _ := name.NewTag("example.com/side/arm:v1")
	path := filepath.Join(t.TempDir(), "arm.tar")
	if err := tarball.WriteToFile(path, tag, img); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	_, err := ImportDockerArchive(path, "")
	if err == nil || !strings.Contains(err.Error(), "requires linux/amd64") {
		t.Fatalf("err = %v, want linux/amd64 rejection", err)
	}
}

func TestImportDockerArchiveMissingFile(t *testing.T) {
	if _, err := ImportDockerArchive(filepath.Join(t.TempDir(), "absent.tar"), ""); err == nil {
		t.Fatal("missing archive accepted")
	}
}
