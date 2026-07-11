# Go SDK

The official Go client: a zero-dependency module (stdlib only), versioned
independently of the daemon with `sdk/vX.Y.Z` tags.

```sh
go get github.com/gnana997/crucible/sdk
```

```go
import (
    crucible "github.com/gnana997/crucible/sdk"
    "github.com/gnana997/crucible/sdk/api"
    "github.com/gnana997/crucible/sdk/wire"
)
```

Full API docs: [pkg.go.dev/github.com/gnana997/crucible/sdk](https://pkg.go.dev/github.com/gnana997/crucible/sdk).

## Client setup

```go
cr := crucible.New("127.0.0.1:7878") // keyless loopback daemon (dev)

cr = crucible.New("daemon.internal:7878",
    crucible.WithToken(os.Getenv("CRUCIBLE_TOKEN"))) // production: TLS + key
```

`New` accepts `host:port` or a full `http(s)://` URL. The token is a daemon
API key (see [authentication](../api.md#authentication)); treat the SDK as
server-side only; a key grants control of the host's microVMs.

## Sandboxes

```go
// Boot from an OCI image (pulled and converted on demand, then cached):
sb, err := cr.CreateSandbox(ctx, api.CreateSandboxRequest{
    Image:     &api.ImageRef{OCI: "python:3.12-alpine"},
    VCPUs:     2,
    MemoryMiB: 512,
})

// Or from a configured rootfs profile / the daemon default:
sb, err = cr.CreateSandbox(ctx, api.CreateSandboxRequest{})

defer cr.DeleteSandbox(ctx, sb.ID)
```

Lists are pages, a shape that stays stable when a future control-plane
adds cursors:

```go
page, err := cr.ListSandboxes(ctx)
for _, s := range page.Items { fmt.Println(s.ID, s.Profile) }
```

## Flat methods and handles

Every REST route has a flat method on `Client` returning the full wire
DTOs. **Handles** are chaining sugar over them: cheap value structs, no
hidden round-trips:

```go
sbx := cr.Sandbox(sb.ID)

res, err := sbx.Exec(ctx, wire.ExecRequest{Cmd: []string{"pytest", "-q"}},
    os.Stdout, os.Stderr)

snap, err := sbx.Snapshot(ctx)      // Snapshot handle
forks, err := snap.Fork(ctx, 8)     // []Sandbox handles
```

Rule of thumb: handles for chaining, flat methods when you need the
response body (`Client.Fork` returns the full `[]api.SandboxResponse`
with network details; `snap.Fork` returns handles).

## Exec: streaming and interactive

One-shot exec streams stdout/stderr to `io.Writer`s as the command runs
and returns the exit result:

```go
res, err := sbx.Exec(ctx, wire.ExecRequest{
    Cmd:        []string{"python3", "train.py"},
    Env:        map[string]string{"EPOCHS": "10"},
    TimeoutSec: 600, // SIGKILL + TimedOut=true when exceeded
}, os.Stdout, os.Stderr)

fmt.Println(res.ExitCode, res.DurationMs, res.Usage.PeakMemoryBytes)
```

`res.Usage` carries per-exec CPU, memory, and I/O counters; it is nil when the
process never started. Failures *after* the stream commits (VM died
mid-run) arrive as `ExitCode -1` with `res.Error` set, never as a broken
stream.

Interactive exec is a full-duplex session: live stdin, persistent
`cwd`/env for the life of the process (what `crucible shell` uses):

```go
res, err := sbx.ExecInteractive(ctx, wire.ExecRequest{Cmd: []string{"/bin/sh"}},
    os.Stdin, os.Stdout, os.Stderr)
```

Cancel the context to kill the guest command. (Under the hood this is a
hijacked connection; the [wire doc](../wire.md) has both transports.)

## Files in and out

```go
// Push a tar stream, extracted beneath /app in the guest (path-escape safe):
result, err := sbx.CopyTo(ctx, "/app", tarReader)

// Read one file back, capped at 1 MiB:
data, err := sbx.ReadFile(ctx, "/app/output.json", 1<<20)
```

The CLI's `crucible cp` builds the tar for you; from Go, use
`archive/tar` over your sources.

## Snapshot → fork: the agent fan-out

The pattern crucible exists for: pay for setup once, explore N branches
from the exact same warm state:

```go
sbx := cr.Sandbox(sb.ID)
_, err := sbx.Exec(ctx, wire.ExecRequest{
    Cmd: []string{"pip", "install", "-r", "requirements.txt"},
}, nil, nil)

snap, err := sbx.Snapshot(ctx)   // capture the warm state
forks, err := snap.Fork(ctx, 8)  // 8 copies, lazy memory, ~100ms each

var wg sync.WaitGroup
for i, f := range forks {
    wg.Add(1)
    go func() {
        defer wg.Done()
        defer f.Delete(ctx)
        f.Exec(ctx, wire.ExecRequest{Cmd: []string{"python3", "attempt.py", strconv.Itoa(i)}}, nil, nil)
    }()
}
wg.Wait()
```

Every fork wakes with fresh entropy and identity
([clone-safety](../architecture.md#snapshot-and-fork)): divergent RNG,
unique hostname, its own network address.

## Durable apps

An **app** is a named workload the daemon keeps alive and re-creates from spec
after a restart ([apps.md](../apps.md)). The SDK has flat methods plus an `App`
handle whose `Exec`/`Logs` resolve the app's current instance for you:

```go
_, err := cr.CreateApp(ctx, api.CreateAppRequest{AppSpec: api.AppSpec{
    Name:    "web",
    Image:   &api.ImageRef{OCI: "nginx:alpine"},
    Publish: []api.PortMapping{{HostPort: 8080, GuestPort: 80}},
    Restart: wire.RestartPolicy{Policy: wire.RestartAlways},
    Health:  &api.HealthCheck{Type: "http", Path: "/", Port: 80},
}})

apps, _ := cr.ListApps(ctx)            // Page[api.AppResponse]
app := cr.App("web")
status, _ := app.Get(ctx)              // desired + observed (phase, health, restarts)
res, _ := app.Exec(ctx, wire.ExecRequest{Cmd: []string{"nginx", "-t"}}, os.Stdout, os.Stderr)
_ = app.Delete(ctx)
```

`app.Exec`/`app.Logs` error when the app has no running instance (pending,
stopped, or crash-looping).

## Services

Run a long-lived entrypoint under the guest supervisor (Docker-style
restart policies):

```go
st, err := sbx.ConfigureService(ctx, wire.ServiceSpec{
    Cmd:     []string{"node", "server.js"},
    Restart: wire.RestartPolicy{Policy: wire.RestartOnFailure, MaxRetries: 3},
})
st, err = sbx.StartService(ctx)
logs, err := sbx.ServiceLogs(ctx, 0, 0) // ring-buffer cursor API
```

Publish guest ports to the host with `api.CreateSandboxRequest.Publish`
(`docker -p` semantics).

## Errors

Every daemon error is a structured `*crucible.Error` (`Status`,
`Message`, reserved `Code`), and the common statuses also match
sentinels:

```go
_, err := cr.GetSandbox(ctx, id)
switch {
case errors.Is(err, crucible.ErrNotFound):     // 404: gone or never existed
case errors.Is(err, crucible.ErrUnauthorized): // 401: bad/missing key
case errors.Is(err, crucible.ErrPolicyDenied): // 403: token's policy forbids it
}

var de *crucible.Error
if errors.As(err, &de) { log.Printf("daemon said %d: %s", de.Status, de.Message) }
```

## Versioning

The module is tagged `sdk/vX.Y.Z`, independent of daemon releases, and is
pre-1.0 (surface may move between minors). The wire contract itself is
frozen; see the [wire protocol](../wire.md) and the
[API reference](/api-reference).
