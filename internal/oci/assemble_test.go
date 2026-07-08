package oci

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// ---- crafting helpers -------------------------------------------------------

type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	uid, gid int
	uname    string
	content  string
	link     string
}

func writeTestTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if e.typeflag == 0 {
			e.typeflag = tar.TypeReg
		}
		if e.mode == 0 {
			e.mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Uid:      e.uid,
			Gid:      e.gid,
			Uname:    e.uname,
			Linkname: e.link,
			Size:     int64(len(e.content)),
			ModTime:  time.Unix(1_700_000_000, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func layerOf(t *testing.T, entries []tarEntry) v1.Layer {
	t.Helper()
	raw := writeTestTar(t, entries)
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	if err != nil {
		t.Fatalf("LayerFromOpener: %v", err)
	}
	return layer
}

func acquiredFromLayers(t *testing.T, layers ...v1.Layer) *Acquired {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	cf = cf.DeepCopy()
	cf.OS = "linux"
	cf.Architecture = "amd64"
	cf.Config = v1.Config{Entrypoint: []string{"/bin/app"}}
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}
	acq, err := finishAcquire(img, "test-image")
	if err != nil {
		t.Fatalf("finishAcquire: %v", err)
	}
	return acq
}

type outEntry struct {
	hdr  tar.Header
	data []byte
}

func readOutput(t *testing.T, r io.Reader) []outEntry {
	t.Helper()
	var out []outEntry
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read output tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read output body %q: %v", hdr.Name, err)
		}
		out = append(out, outEntry{hdr: *hdr, data: data})
	}
	return out
}

func findEntry(entries []outEntry, name string) *outEntry {
	for i := range entries {
		if entries[i].hdr.Name == name {
			return &entries[i]
		}
	}
	return nil
}

var fixedNow = func() time.Time { return time.Unix(1_750_000_000, 0) }

func assembleOK(t *testing.T, acq *Acquired, opts AssembleOptions) ([]outEntry, *AssembleStats) {
	t.Helper()
	if opts.Agent == nil {
		opts.Agent = []byte("FAKE-AGENT-BINARY")
	}
	if opts.Now == nil {
		opts.Now = fixedNow
	}
	if opts.ConverterVersion == "" {
		opts.ConverterVersion = "test-converter"
	}
	var buf bytes.Buffer
	stats, err := Assemble(acq, &buf, opts)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return readOutput(t, &buf), stats
}

// ---- tests ------------------------------------------------------------------

func TestAssembleBasicFidelityAndInjection(t *testing.T) {
	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/app", mode: 0o755, uid: 0, gid: 0, content: "#!ELF fake"},
		{name: "home/user/data.txt", mode: 0o600, uid: 1234, gid: 5678, uname: "someuser", content: "hello"},
		{name: "bin/link-to-app", typeflag: tar.TypeSymlink, link: "/bin/app"},
	}))

	out, stats := assembleOK(t, acq, AssembleOptions{})

	app := findEntry(out, "bin/app")
	if app == nil || string(app.data) != "#!ELF fake" || app.hdr.Mode != 0o755 {
		t.Fatalf("bin/app = %+v", app)
	}
	data := findEntry(out, "home/user/data.txt")
	if data == nil || data.hdr.Uid != 1234 || data.hdr.Gid != 5678 {
		t.Fatalf("uid/gid not preserved: %+v", data)
	}
	if data.hdr.Uname != "" {
		t.Errorf("Uname survived (%q); numeric ownership must be authoritative", data.hdr.Uname)
	}
	sym := findEntry(out, "bin/link-to-app")
	if sym == nil || sym.hdr.Typeflag != tar.TypeSymlink || sym.hdr.Linkname != "/bin/app" {
		t.Fatalf("symlink not preserved verbatim: %+v", sym)
	}

	// Injection: last three entries, fixed order.
	n := len(out)
	if n < 3 || out[n-3].hdr.Name != "crucible" || out[n-2].hdr.Name != "crucible/crucible-agent" || out[n-1].hdr.Name != "crucible/run.json" {
		t.Fatalf("injected entries not last: %v", entryNames(out))
	}
	if string(out[n-2].data) != "FAKE-AGENT-BINARY" || out[n-2].hdr.Mode != 0o755 {
		t.Errorf("agent injection wrong: mode %o data %q", out[n-2].hdr.Mode, out[n-2].data)
	}
	var rc RunConfig
	if err := json.Unmarshal(out[n-1].data, &rc); err != nil {
		t.Fatalf("run.json unparseable: %v", err)
	}
	if rc.Version != RunConfigVersion || rc.Entrypoint[0] != "/bin/app" {
		t.Errorf("run.json content: %+v", rc)
	}
	if rc.ConvertedAtUnixMs != fixedNow().UTC().UnixMilli() || rc.ConverterVersion != "test-converter" {
		t.Errorf("run.json stamps: at=%d ver=%q", rc.ConvertedAtUnixMs, rc.ConverterVersion)
	}

	wantContent := int64(len("#!ELF fake")+len("hello")+len("FAKE-AGENT-BINARY")) + int64(len(out[n-1].data))
	if stats.ContentBytes != wantContent {
		t.Errorf("ContentBytes = %d, want %d", stats.ContentBytes, wantContent)
	}
	if stats.Entries != len(out) {
		t.Errorf("Entries = %d, out has %d", stats.Entries, len(out))
	}
}

func entryNames(out []outEntry) []string {
	names := make([]string, len(out))
	for i := range out {
		names[i] = out[i].hdr.Name
	}
	return names
}

func TestAssembleAppliesWhiteouts(t *testing.T) {
	lower := layerOf(t, []tarEntry{
		{name: "keep.txt", content: "keep"},
		{name: "gone.txt", content: "gone"},
	})
	upper := layerOf(t, []tarEntry{
		{name: ".wh.gone.txt"},
	})
	out, _ := assembleOK(t, acquiredFromLayers(t, lower, upper), AssembleOptions{})

	if findEntry(out, "gone.txt") != nil {
		t.Error("whiteouted file survived flattening")
	}
	if findEntry(out, "keep.txt") == nil {
		t.Error("unrelated file lost")
	}
	for _, e := range out {
		if strings.Contains(e.hdr.Name, ".wh.") {
			t.Errorf("whiteout marker leaked into artifact: %s", e.hdr.Name)
		}
	}
}

func TestAssembleAppliesOpaqueDirs(t *testing.T) {
	lower := layerOf(t, []tarEntry{
		{name: "cfg/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "cfg/old.conf", content: "old"},
	})
	upper := layerOf(t, []tarEntry{
		{name: "cfg/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "cfg/.wh..wh..opq"},
		{name: "cfg/new.conf", content: "new"},
	})
	out, _ := assembleOK(t, acquiredFromLayers(t, lower, upper), AssembleOptions{})

	if findEntry(out, "cfg/old.conf") != nil {
		t.Error("opaque dir did not clear lower-layer contents")
	}
	if e := findEntry(out, "cfg/new.conf"); e == nil || string(e.data) != "new" {
		t.Error("upper-layer file lost")
	}
}

func TestAssemblePreservesSetuid(t *testing.T) {
	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "usr/bin/sudo", mode: 0o4755, content: "fake-sudo"},
	}))
	out, _ := assembleOK(t, acq, AssembleOptions{})
	e := findEntry(out, "usr/bin/sudo")
	if e == nil || e.hdr.Mode != 0o4755 {
		t.Fatalf("setuid bit lost: %+v", e)
	}
}

func TestAssembleSkipsDeviceNodesKeepsFifos(t *testing.T) {
	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "dev/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "dev/console", typeflag: tar.TypeChar},
		{name: "dev/sda", typeflag: tar.TypeBlock},
		{name: "run/queue", typeflag: tar.TypeFifo},
	}))
	out, stats := assembleOK(t, acq, AssembleOptions{})

	if findEntry(out, "dev/console") != nil || findEntry(out, "dev/sda") != nil {
		t.Error("device node entered the artifact")
	}
	if stats.SkippedDevices != 2 {
		t.Errorf("SkippedDevices = %d, want 2", stats.SkippedDevices)
	}
	if findEntry(out, "run/queue") == nil {
		t.Error("fifo dropped")
	}
}

func TestAssembleRejectsTraversalAndAbsolute(t *testing.T) {
	for _, tc := range []struct{ name, entry string }{
		{"dotdot", "../evil"},
		{"nested dotdot", "a/../../evil"},
		{"absolute", "/etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acq := acquiredFromLayers(t, layerOf(t, []tarEntry{{name: tc.entry, content: "x"}}))
			var buf bytes.Buffer
			_, err := Assemble(acq, &buf, AssembleOptions{Agent: []byte("A"), Now: fixedNow})
			if err == nil {
				t.Fatalf("hostile entry %q accepted", tc.entry)
			}
		})
	}
}

func TestAssembleHardlinks(t *testing.T) {
	// Valid hardlink passes with a normalized target.
	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "a.txt", content: "data"},
		{name: "l.txt", typeflag: tar.TypeLink, link: "./a.txt"},
	}))
	out, _ := assembleOK(t, acq, AssembleOptions{})
	l := findEntry(out, "l.txt")
	if l == nil || l.hdr.Typeflag != tar.TypeLink || l.hdr.Linkname != "a.txt" {
		t.Fatalf("hardlink not preserved/normalized: %+v", l)
	}

	// A hardlink escaping the root fails the conversion.
	bad := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "x", content: "x"},
		{name: "evil", typeflag: tar.TypeLink, link: "../../etc/passwd"},
	}))
	var buf bytes.Buffer
	if _, err := Assemble(bad, &buf, AssembleOptions{Agent: []byte("A"), Now: fixedNow}); err == nil {
		t.Fatal("escaping hardlink accepted")
	}
}

func TestAssembleReservedNamespaceInjectionWins(t *testing.T) {
	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "crucible/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "crucible/run.json", content: "EVIL"},
		{name: "crucible/crucible-agent", content: "EVIL-AGENT"},
		{name: "app/ok.txt", content: "fine"},
	}))
	out, stats := assembleOK(t, acq, AssembleOptions{})

	if stats.SkippedReserved != 3 {
		t.Errorf("SkippedReserved = %d, want 3", stats.SkippedReserved)
	}
	var runJSONs, agents []*outEntry
	for i := range out {
		switch out[i].hdr.Name {
		case "crucible/run.json":
			runJSONs = append(runJSONs, &out[i])
		case "crucible/crucible-agent":
			agents = append(agents, &out[i])
		}
	}
	if len(runJSONs) != 1 || len(agents) != 1 {
		t.Fatalf("reserved entries duplicated: %d run.json, %d agent", len(runJSONs), len(agents))
	}
	if string(agents[0].data) != "FAKE-AGENT-BINARY" {
		t.Error("image-provided agent won over the injected one")
	}
	var rc RunConfig
	if err := json.Unmarshal(runJSONs[0].data, &rc); err != nil {
		t.Error("surviving run.json is not ours")
	}
}

func TestAssembleCaps(t *testing.T) {
	files := []tarEntry{
		{name: "a", content: "0123456789"},
		{name: "b", content: "0123456789"},
		{name: "c", content: "0123456789"},
	}
	acq := acquiredFromLayers(t, layerOf(t, files))

	var buf bytes.Buffer
	if _, err := Assemble(acq, &buf, AssembleOptions{Agent: []byte("A"), Now: fixedNow, MaxEntries: 2}); err == nil ||
		!strings.Contains(err.Error(), "entry cap") {
		t.Errorf("entry cap not enforced: %v", err)
	}
	buf.Reset()
	if _, err := Assemble(acq, &buf, AssembleOptions{Agent: []byte("A"), Now: fixedNow, MaxContentBytes: 15}); err == nil ||
		!strings.Contains(err.Error(), "byte cap") {
		t.Errorf("content cap not enforced: %v", err)
	}
	buf.Reset()
	two := acquiredFromLayers(t, layerOf(t, files[:1]), layerOf(t, files[1:2]))
	if _, err := Assemble(two, &buf, AssembleOptions{Agent: []byte("A"), Now: fixedNow, MaxLayers: 1}); err == nil ||
		!strings.Contains(err.Error(), "layers") {
		t.Errorf("layer cap not enforced: %v", err)
	}
}

func TestAssembleRequiresAgent(t *testing.T) {
	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{{name: "a", content: "x"}}))
	var buf bytes.Buffer
	if _, err := Assemble(acq, &buf, AssembleOptions{}); err == nil {
		t.Fatal("assemble without an agent accepted")
	}
}

func TestAssembleDeterministic(t *testing.T) {
	acq := acquiredFromLayers(t, layerOf(t, []tarEntry{
		{name: "app/x", content: "one"},
		{name: "app/y", content: "two"},
	}))
	opts := AssembleOptions{Agent: []byte("A"), Now: fixedNow, ConverterVersion: "v"}
	var b1, b2 bytes.Buffer
	if _, err := Assemble(acq, &b1, opts); err != nil {
		t.Fatalf("first assemble: %v", err)
	}
	if _, err := Assemble(acq, &b2, opts); err != nil {
		t.Fatalf("second assemble: %v", err)
	}
	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Error("assembly is not deterministic for identical inputs")
	}
}

// TestAssembleHardlinkToWhiteoutedTarget pins how the known ggcr edge
// (go-containerregistry#977) surfaces: a hardlink whose target a later
// layer deletes cannot be represented in a flattened tar. Whatever ggcr
// does — error out or drop the link — the conversion must not emit a
// dangling hardlink silently.
func TestAssembleHardlinkToWhiteoutedTarget(t *testing.T) {
	lower := layerOf(t, []tarEntry{
		{name: "data", content: "payload"},
		{name: "alias", typeflag: tar.TypeLink, link: "data"},
	})
	upper := layerOf(t, []tarEntry{
		{name: ".wh.data"},
	})
	acq := acquiredFromLayers(t, lower, upper)

	var buf bytes.Buffer
	_, err := Assemble(acq, &buf, AssembleOptions{Agent: []byte("A"), Now: fixedNow})
	if err != nil {
		return // surfaced as a clear conversion error — acceptable
	}
	// If ggcr tolerated it, the artifact must not contain a hardlink
	// pointing at a path that doesn't exist.
	out := readOutput(t, &buf)
	if l := findEntry(out, "alias"); l != nil && l.hdr.Typeflag == tar.TypeLink {
		if findEntry(out, l.hdr.Linkname) == nil {
			t.Fatalf("dangling hardlink %q -> %q in artifact", l.hdr.Name, l.hdr.Linkname)
		}
	}
}

func TestNormalizeEntryName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		skip bool
		err  bool
	}{
		{in: "foo/bar", want: "foo/bar"},
		{in: "./foo", want: "foo"},
		{in: "foo/", want: "foo"},
		{in: "a/../b", want: "b"},
		{in: ".", skip: true},
		{in: "./", skip: true},
		{in: "", skip: true},
		{in: "..", err: true},
		{in: "../x", err: true},
		{in: "a/../../x", err: true},
		{in: "/abs", err: true},
	}
	for _, tc := range cases {
		got, skip, err := normalizeEntryName(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("normalize(%q): no error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalize(%q): %v", tc.in, err)
			continue
		}
		if skip != tc.skip || (!skip && got != tc.want) {
			t.Errorf("normalize(%q) = (%q, skip=%v), want (%q, skip=%v)", tc.in, got, skip, tc.want, tc.skip)
		}
	}
}
