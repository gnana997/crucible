// Command openapi-gen emits the crucible daemon's OpenAPI 3.0 spec by reflecting
// the wire DTOs in sdk/api (and the sdk/wire + policy types they reference)
// and declaring one operation per REST route.
//
// The Go types are the source of truth: request/response schemas are reflected
// from the structs, so the spec cannot drift from them. Operations (paths,
// methods, params, status codes, auth) are declared here explicitly; the
// coverage test (coverage_test.go) asserts every route registered in
// internal/daemon/routes.go is either declared here or consciously excluded.
//
// Regenerate with `make openapi` (or `go run ./cmd/openapi-gen`). The output
// feeds both the SDK codegen and the rendered API reference on the docs site.
//
// Two things OpenAPI can't model, handled honestly rather than faked:
//   - exec and service-logs stream a binary frame protocol (sdk/wire); they are
//     declared as octet-stream responses with the frame format described in prose.
//   - the files push/import request bodies are raw tar streams; described in prose.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	jsonschema "github.com/swaggest/jsonschema-go"
	oapi "github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi3"

	"github.com/gnana997/crucible/internal/policy"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

// --- path/query parameter shapes (reflected as OpenAPI parameters) ----------

type idParam struct {
	ID string `path:"id" description:"Sandbox ID, e.g. sbx_ab12cd34."`
}
type appNameParam struct {
	Name string `path:"name" description:"App name (a DNS label, e.g. web)."`
}
type snapIDParam struct {
	ID string `path:"id" description:"Snapshot ID, e.g. snap_ab12cd34."`
}
type refParam struct {
	Ref string `path:"ref" description:"Image reference (nginx:alpine) or converted digest."`
}
type healthResponse struct {
	Status string `json:"status" example:"ok"`
}

// Composite request shapes: an embedded body struct (its json fields become the
// request body) plus path/query parameter fields.
type execReq struct {
	idParam
	Stdin string `query:"stdin" description:"Set to \"1\" for an interactive, full-duplex exec (hijacked stream)."`
	wire.ExecRequest
}
type putFilesReq struct {
	idParam
	Path string `query:"path" required:"true" description:"Guest destination directory the tar is extracted beneath."`
}
type getFileReq struct {
	idParam
	Path     string `query:"path" required:"true" description:"Guest file path to read."`
	MaxBytes int    `query:"max_bytes" description:"Cap on bytes returned (default 10 MiB)."`
}
type configureServiceReq struct {
	idParam
	wire.ServiceSpec
}
type serviceStopReq struct {
	idParam
	wire.ServiceStopRequest
}
type logsReq struct {
	idParam
	Since  int64  `query:"since" description:"Byte offset to read from; -1 (default) tails the recent log."`
	Source string `query:"source" description:"Filter by source: service | exec | all (default all)."`
}
type forkReq struct {
	snapIDParam
	Count int `query:"count" description:"Number of sandboxes to fork (default 1). The body's count wins when both are set."`
	api.ForkRequest
}
type importReq struct {
	Tag string `query:"tag" description:"Which image inside a multi-image docker-save archive to import."`
}

// stripPkgPrefix cleans swaggest's package-qualified schema names
// (ApiCreateSandboxRequest, AgentwireServiceSpec, PolicyWhoami) down to the Go
// type name, so the public spec and generated SDK types read naturally.
func stripPkgPrefix(_ reflect.Type, def string) string {
	for _, p := range []string{"Api", "Agentwire", "Policy"} {
		if strings.HasPrefix(def, p) && len(def) > len(p) {
			return def[len(p):]
		}
	}
	return def
}

// buildReflector declares every operation and returns the reflector. Shared by
// main() (to marshal) and coverage_test.go (to enumerate declared routes).
func buildReflector() *openapi3.Reflector {
	r := openapi3.NewReflector()
	r.DefaultOptions = append(r.DefaultOptions, jsonschema.InterceptDefName(stripPkgPrefix))

	spec := r.SpecEns()
	spec.Info.
		WithTitle("crucible").
		WithVersion("0.4.1").
		WithDescription("REST API for the crucible daemon, a Firecracker microVM sandbox runtime. " +
			"The daemon is the contract every SDK mirrors. Auth is a bearer token " +
			"(`Authorization: Bearer <key>`); `/healthz` is always exempt, and a loopback daemon " +
			"with no keys configured serves unauthenticated. The exec and service-log endpoints " +
			"stream a binary frame protocol OpenAPI cannot model — see the guides for the " +
			"frame format.")
	spec.SetHTTPBearerTokenSecurity("bearerAuth", "", "Daemon API key. Omit on a keyless loopback daemon.")

	must := func(oc oapi.OperationContext, err error) oapi.OperationContext {
		if err != nil {
			log.Fatalf("new operation: %v", err)
		}
		return oc
	}
	add := func(oc oapi.OperationContext) {
		if err := r.AddOperation(oc); err != nil {
			log.Fatalf("add operation: %v", err)
		}
	}
	errs := func(oc oapi.OperationContext, statuses ...int) {
		for _, st := range statuses {
			oc.AddRespStructure(api.ErrorResponse{}, oapi.WithHTTPStatus(st))
		}
	}

	// jsonOp: a JSON request/response operation.
	jsonOp := func(method, path, id, tag, summary, desc string, req, resp any, respStatus int, errStatuses ...int) {
		oc := must(r.NewOperationContext(method, path))
		oc.SetID(id)
		oc.SetTags(tag)
		oc.SetSummary(summary)
		if desc != "" {
			oc.SetDescription(desc)
		}
		if req != nil {
			oc.AddReqStructure(req)
		}
		if resp != nil {
			oc.AddRespStructure(resp, oapi.WithHTTPStatus(respStatus))
		} else {
			oc.AddRespStructure(nil, oapi.WithHTTPStatus(respStatus)) // e.g. 204 No Content
		}
		errs(oc, errStatuses...)
		add(oc)
	}

	// streamOp: an operation whose response is a raw byte stream (frames or file
	// bytes) that OpenAPI can only type as binary; the semantics live in desc.
	streamOp := func(method, path, id, tag, summary, desc string, req any, errStatuses ...int) {
		oc := must(r.NewOperationContext(method, path))
		oc.SetID(id)
		oc.SetTags(tag)
		oc.SetSummary(summary)
		oc.SetDescription(desc)
		if req != nil {
			oc.AddReqStructure(req)
		}
		oc.AddRespStructure(new([]byte), oapi.WithHTTPStatus(http.StatusOK), oapi.WithContentType("application/octet-stream"))
		errs(oc, errStatuses...)
		add(oc)
	}

	// --- meta ---
	jsonOp(http.MethodGet, "/healthz", "health", "meta", "Liveness check",
		"Always returns 200 and is exempt from auth, so probes work without a key.",
		nil, healthResponse{}, http.StatusOK)
	jsonOp(http.MethodGet, "/whoami", "whoami", "meta", "Effective policy for the caller",
		"Reports whether the token is scoped and, if so, the policy the daemon enforces for it.",
		nil, policy.Whoami{}, http.StatusOK)
	jsonOp(http.MethodGet, "/profiles", "listProfiles", "meta", "List rootfs profiles",
		"The rootfs profiles the daemon can boot sandboxes from.",
		nil, api.ProfilesResponse{}, http.StatusOK)

	// --- apps (durable workloads reconciled into instances; v0.4) ---
	jsonOp(http.MethodPost, "/apps", "createApp", "apps", "Create a durable app",
		"Creates a named app the daemon keeps a healthy instance of, re-creating it "+
			"from spec after a daemon restart. desired_state defaults to \"running\". The "+
			"instance boots asynchronously; the response is the app's initial state.",
		api.CreateAppRequest{}, api.AppResponse{}, http.StatusCreated,
		http.StatusBadRequest, http.StatusConflict, http.StatusForbidden, http.StatusNotImplemented)
	jsonOp(http.MethodGet, "/apps", "listApps", "apps", "List apps",
		"", nil, api.AppListResponse{}, http.StatusOK, http.StatusNotImplemented)
	jsonOp(http.MethodGet, "/apps/{name}", "getApp", "apps", "Get an app",
		"Desired state plus observed status (instance id, phase, health, restarts).",
		appNameParam{}, api.AppResponse{}, http.StatusOK, http.StatusNotFound, http.StatusNotImplemented)
	jsonOp(http.MethodDelete, "/apps/{name}", "deleteApp", "apps", "Delete an app",
		"Removes the app and tears down its instance on the next reconcile.",
		appNameParam{}, nil, http.StatusNoContent, http.StatusNotFound, http.StatusNotImplemented)

	// --- sandboxes ---
	jsonOp(http.MethodPost, "/sandboxes", "createSandbox", "sandboxes", "Create a sandbox",
		"Boots a Firecracker microVM. All fields are optional; an empty body uses the daemon "+
			"defaults. `image` boots from a converted OCI image (pulled on demand — see the images "+
			"endpoints); a daemon without an image store answers 501.",
		api.CreateSandboxRequest{}, api.SandboxResponse{}, http.StatusCreated,
		http.StatusBadRequest, http.StatusForbidden, http.StatusNotImplemented, http.StatusBadGateway, http.StatusInternalServerError)
	jsonOp(http.MethodGet, "/sandboxes", "listSandboxes", "sandboxes", "List sandboxes",
		"", nil, api.ListResponse{}, http.StatusOK)
	jsonOp(http.MethodGet, "/sandboxes/{id}", "getSandbox", "sandboxes", "Get a sandbox",
		"", idParam{}, api.SandboxResponse{}, http.StatusOK, http.StatusBadRequest, http.StatusNotFound)
	jsonOp(http.MethodDelete, "/sandboxes/{id}", "deleteSandbox", "sandboxes", "Destroy a sandbox",
		"", idParam{}, nil, http.StatusNoContent, http.StatusBadRequest, http.StatusNotFound)
	streamOp(http.MethodPost, "/sandboxes/{id}/exec", "execSandbox", "sandboxes", "Run a command (streams frames)",
		"Runs a command in the guest and streams the result as length-prefixed frames "+
			"([type:1][reserved:3][size:4 BE][payload]) where type is stdout, stderr, or exit; the "+
			"exit payload is a JSON ExecResult. Set ?stdin=1 for a hijacked, full-duplex interactive exec.",
		execReq{}, http.StatusBadRequest, http.StatusNotFound)
	// The WebSocket variant of interactive exec — the transport for
	// non-Go SDKs (fetch() cannot speak the hijacked ?stdin=1 stream).
	// OpenAPI cannot model a WebSocket session, so the contract is prose.
	jsonOp(http.MethodGet, "/sandboxes/{id}/exec", "execSandboxWS", "sandboxes",
		"Interactive exec (WebSocket)",
		"WebSocket upgrade for a full-duplex interactive exec. The client's first message is "+
			"the JSON ExecRequest; after that, the concatenated binary message payloads in each "+
			"direction form exactly the same length-prefixed frame stream as POST ?stdin=1 "+
			"(stdin/stdin_close up, stdout/stderr/exit down). Frames may split across messages — "+
			"decode the concatenated stream. Non-WebSocket GETs answer 426.",
		idParam{}, nil, http.StatusSwitchingProtocols, http.StatusBadRequest, http.StatusNotFound)
	jsonOp(http.MethodPost, "/sandboxes/{id}/files", "putFiles", "sandboxes", "Push files into a sandbox",
		"Extracts a tar stream (the request body, application/octet-stream) beneath ?path in the "+
			"guest filesystem; the `crucible cp` push path. Rejects entries that escape the destination.",
		putFilesReq{}, wire.FilesPutResult{}, http.StatusOK, http.StatusBadRequest, http.StatusNotFound)
	streamOp(http.MethodGet, "/sandboxes/{id}/files", "getFile", "sandboxes", "Read a single guest file",
		"Returns the raw bytes of one guest file (?path), capped at ?max_bytes. Content only, guest → "+
			"host; nothing is written host-side.",
		getFileReq{}, http.StatusBadRequest, http.StatusNotFound)

	// --- service (experimental) ---
	jsonOp(http.MethodPut, "/sandboxes/{id}/service", "configureService", "service", "Configure the supervised service",
		"Experimental. Sets and (re)starts the guest's supervised long-lived entrypoint.",
		configureServiceReq{}, wire.ServiceStatus{}, http.StatusOK, http.StatusBadRequest, http.StatusNotFound)
	jsonOp(http.MethodPost, "/sandboxes/{id}/service/start", "startService", "service", "Start the service",
		"", idParam{}, wire.ServiceStatus{}, http.StatusOK, http.StatusNotFound)
	jsonOp(http.MethodPost, "/sandboxes/{id}/service/stop", "stopService", "service", "Stop the service",
		"", serviceStopReq{}, wire.ServiceStatus{}, http.StatusOK, http.StatusNotFound)
	jsonOp(http.MethodPost, "/sandboxes/{id}/service/restart", "restartService", "service", "Restart the service",
		"", idParam{}, wire.ServiceStatus{}, http.StatusOK, http.StatusNotFound)
	jsonOp(http.MethodGet, "/sandboxes/{id}/service", "serviceStatus", "service", "Service status",
		"", idParam{}, wire.ServiceStatus{}, http.StatusOK, http.StatusNotFound)
	streamOp(http.MethodGet, "/sandboxes/{id}/service/logs", "serviceLogs", "service", "Stream service logs",
		"Experimental. Streams the supervised service's stdout/stderr as it is produced.",
		idParam{}, http.StatusNotFound)

	// --- logs ---
	jsonOp(http.MethodGet, "/sandboxes/{id}/logs", "sandboxLogs", "logs", "Read durable sandbox logs",
		"Durable per-sandbox logs (service output + exec activity) that survive the sandbox. "+
			"Returns 501 when the daemon has no log store configured.",
		logsReq{}, api.LogsResponse{}, http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusNotImplemented)

	// --- snapshots & fork ---
	jsonOp(http.MethodPost, "/sandboxes/{id}/snapshot", "createSnapshot", "snapshots", "Snapshot a sandbox",
		"", idParam{}, api.SnapshotResponse{}, http.StatusCreated, http.StatusBadRequest, http.StatusNotFound)
	jsonOp(http.MethodGet, "/snapshots", "listSnapshots", "snapshots", "List snapshots",
		"", nil, api.SnapshotListResponse{}, http.StatusOK)
	jsonOp(http.MethodGet, "/snapshots/{id}", "getSnapshot", "snapshots", "Get a snapshot",
		"", snapIDParam{}, api.SnapshotResponse{}, http.StatusOK, http.StatusBadRequest, http.StatusNotFound)
	jsonOp(http.MethodDelete, "/snapshots/{id}", "deleteSnapshot", "snapshots", "Delete a snapshot",
		"", snapIDParam{}, nil, http.StatusNoContent, http.StatusBadRequest, http.StatusNotFound)
	jsonOp(http.MethodPost, "/snapshots/{id}/fork", "forkSnapshot", "snapshots", "Fork sandboxes from a snapshot",
		"Creates count sandboxes from the snapshot (query param or body; body wins). All-or-nothing: a mid-fork failure rolls back. The optional body's publish maps host ports onto the fork (docker -p semantics); publish requires count 1 because host ports are exclusive.",
		forkReq{}, api.ForkResponse{}, http.StatusCreated, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound)

	// --- images (experimental) ---
	jsonOp(http.MethodPost, "/images", "pullImage", "images", "Pull and convert an OCI image",
		"Experimental. Pulls a registry image and converts it to a bootable rootfs. Returns 501 when "+
			"image support is not enabled (set --image-dir).",
		api.PullImageRequest{}, api.ImageResponse{}, http.StatusCreated,
		http.StatusBadRequest, http.StatusNotImplemented, http.StatusBadGateway)
	jsonOp(http.MethodPost, "/images/import", "importImage", "images", "Import a docker-save archive",
		"Experimental. Imports an image from a docker-save tar on the request body (application/"+
			"octet-stream); ?tag selects one image in a multi-image archive.",
		importReq{}, api.ImageResponse{}, http.StatusCreated, http.StatusNotImplemented, http.StatusBadGateway)
	jsonOp(http.MethodGet, "/images", "listImages", "images", "List cached images",
		"", nil, api.ImageListResponse{}, http.StatusOK, http.StatusNotImplemented)
	jsonOp(http.MethodGet, "/images/{ref}", "getImage", "images", "Get a cached image",
		"", refParam{}, api.ImageResponse{}, http.StatusOK, http.StatusNotFound, http.StatusNotImplemented)
	jsonOp(http.MethodDelete, "/images/{ref}", "deleteImage", "images", "Delete a cached image",
		"", refParam{}, nil, http.StatusNoContent, http.StatusNotFound, http.StatusNotImplemented)

	return r
}

func main() {
	out := flag.String("out", "docs/openapi.json", "output path for the spec")
	flag.Parse()

	r := buildReflector()
	data, err := json.MarshalIndent(r.SpecEns(), "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		log.Fatalf("write: %v", err)
	}
	log.Printf("wrote %s (%d bytes)", *out, len(data))
}
