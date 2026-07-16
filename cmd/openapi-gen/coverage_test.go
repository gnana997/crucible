package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// routesExcludedFromSpec are daemon routes intentionally absent from the public
// OpenAPI spec, each with the reason. This is the escape hatch: a route is either
// declared in buildReflector or listed here on purpose — never silently missing.
var routesExcludedFromSpec = map[string]string{
	"GET /metrics": "Prometheus scrape endpoint; not SDK-facing (text exposition, not JSON)",
	// Sandbox-level in-place sleep/wake: low-level primitive,
	// not in the public API surface while scale-to-zero is under construction
	// (the product surface will be app-level sleep/wake).
	"POST /sandboxes/{id}/sleep": "scale-to-zero primitive; not yet public API",
	"POST /sandboxes/{id}/wake":  "scale-to-zero primitive; not yet public API",
	"POST /apps/{name}/sleep":    "scale-to-zero; not yet public API (under construction)",
	"POST /apps/{name}/wake":     "scale-to-zero; not yet public API (under construction)",
	"POST /apps/{name}/stop":     "desired-state lifecycle; not yet public API (under construction)",
	"POST /apps/{name}/start":    "desired-state lifecycle; not yet public API (under construction)",
	// Packet capture streams a raw pcap (application/vnd.tcpdump.pcap), not JSON —
	// not modelable as an OpenAPI operation (like /metrics). SDK: Client.Capture.
	"GET /sandboxes/{id}/capture": "binary pcap stream (not JSON); SDK Client.Capture, docs/network.md",
	"GET /admin/backup":           "binary tar.gz stream (not JSON); SDK Client.AdminBackup, docs/backups.md",
	"GET /backups/{id}/export":    "binary backup stream (not JSON); SDK Client.ExportBackup, docs/backups.md",
	"POST /backups/import":        "binary backup upload stream (not JSON body); SDK Client.ImportBackup, docs/backups.md",
}

// TestOpenAPICoversAllRoutes is the drift guard for the reflection-based generator.
// Schemas can't drift (they're reflected from the DTOs), but operations are declared
// by hand — so a new route in routes.go could be forgotten. This fails the build
// until every registered route is either documented or consciously excluded, and
// flags stale operations/exclusions that no longer match a route.
func TestOpenAPICoversAllRoutes(t *testing.T) {
	declared := declaredOps(t)
	registered := registeredRoutes(t)

	for route := range registered {
		if _, excluded := routesExcludedFromSpec[route]; excluded {
			continue
		}
		if !declared[route] {
			t.Errorf("route %q is registered in routes.go but has no OpenAPI operation.\n"+
				"Declare it in buildReflector(), or add it to routesExcludedFromSpec with a reason.", route)
		}
	}
	for route := range declared {
		if !registered[route] {
			t.Errorf("OpenAPI declares operation %q, but no such route is registered in routes.go.", route)
		}
	}
	for route := range routesExcludedFromSpec {
		if !registered[route] {
			t.Errorf("routesExcludedFromSpec lists %q, but it is no longer registered in routes.go.", route)
		}
	}
}

// declaredOps returns the "METHOD /path" set the generated spec documents.
func declaredOps(t *testing.T) map[string]bool {
	t.Helper()
	data, err := json.Marshal(buildReflector().SpecEns())
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	methods := map[string]bool{"get": true, "post": true, "put": true, "delete": true, "patch": true, "head": true, "options": true}
	out := map[string]bool{}
	for path, item := range doc.Paths {
		for key := range item {
			if methods[key] {
				out[strings.ToUpper(key)+" "+path] = true
			}
		}
	}
	return out
}

// registeredRoutes parses the "METHOD /path" set actually wired in routes.go.
// Static parse (not mux introspection) because stdlib ServeMux exposes no way to
// list its registered patterns.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	routesPath := filepath.Join(filepath.Dir(self), "..", "..", "internal", "daemon", "routes.go")
	src, err := os.ReadFile(routesPath)
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	re := regexp.MustCompile(`mux\.(?:HandleFunc|Handle)\("([A-Z]+) ([^"]+)"`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out[m[1]+" "+m[2]] = true
	}
	if len(out) == 0 {
		t.Fatal("parsed zero routes from routes.go — the registration pattern likely changed; update the regex")
	}
	return out
}
