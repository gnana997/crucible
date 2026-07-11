# crucible Go SDK

The official Go client for the [crucible](https://github.com/gnana997/crucible)
daemon: Firecracker microVM sandboxes for untrusted / AI-generated code.
Zero dependencies beyond the standard library.

```sh
go get github.com/gnana997/crucible/sdk
```

```go
import (
	"context"
	"os"

	crucible "github.com/gnana997/crucible/sdk"
	"github.com/gnana997/crucible/sdk/api"
	"github.com/gnana997/crucible/sdk/wire"
)

func main() {
	cr := crucible.New("127.0.0.1:7878", crucible.WithToken(os.Getenv("CRUCIBLE_TOKEN")))
	ctx := context.Background()

	sb, err := cr.CreateSandbox(ctx, api.CreateSandboxRequest{
		Image: &api.ImageRef{OCI: "python:3.12-alpine"},
	})
	if err != nil { /* … */ }

	sbx := cr.Sandbox(sb.ID)
	defer sbx.Delete(ctx)

	res, err := sbx.Exec(ctx, wire.ExecRequest{Cmd: []string{"python3", "-c", "print(6*7)"}},
		os.Stdout, os.Stderr)
	if err != nil { /* … */ }
	_ = res.ExitCode // 0; res.Usage carries CPU/memory/IO counters

	// Snapshot the warm state, then fan out copies in ~100ms each:
	snap, _ := sbx.Snapshot(ctx)
	forks, _ := snap.Fork(ctx, 8)
	_ = forks
}
```

## Shape of the API

- **Flat methods** on `Client` map 1:1 to the daemon's REST routes and
  return the full wire DTOs (`api.SandboxResponse`, …).
- **Handles** (`cr.Sandbox(id)`, `cr.SnapshotHandle(id)`) are chaining
  sugar over them: cheap value structs, no hidden round-trips.
- **Lists** return `Page[T]` (`.Items`, plus a `NextCursor` reserved for a
  future control-plane; always empty against a single-node daemon).
- **Errors** are structured: every daemon error is a `*crucible.Error`
  (`Status`, `Message`, reserved `Code`), and 404/401/403 also match the
  `ErrNotFound` / `ErrUnauthorized` / `ErrPolicyDenied` sentinels via
  `errors.Is`.
- **Exec streams**: `Exec` writes stdout/stderr to `io.Writer`s as the
  command runs; `ExecInteractive` gives a full-duplex session (live
  stdin, persistent cwd/env). The frame protocol underneath is specified
  in [docs/wire.md](../docs/wire.md).

## Security model

A daemon bearer token grants control of that host's microVMs, so treat this
as a **server-side** library and never embed a token in code shipped to
browsers or other untrusted clients. A loopback daemon with no keys
configured accepts unauthenticated requests (development convenience).

## Versioning

This module versions independently of the daemon, with tags prefixed
`sdk/` (e.g. `sdk/v0.1.0`). It is pre-1.0: the surface can still move
between minor versions. The wire contract itself is frozen; see
[docs/wire.md](../docs/wire.md) and the generated
[openapi.json](../docs/openapi.json).
