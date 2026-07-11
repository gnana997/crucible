# crucible TypeScript SDK (scaffold)

A typed, zero-runtime-dependency `fetch` client for the
[crucible](https://github.com/gnana997/crucible) daemon. Server-side
runtimes only (Node ≥ 22.18, Bun, Deno): a daemon token grants control of
the host's microVMs and must never ship to a browser.

**Status: scaffold, not yet published to npm.** The core works (typed
client, streaming exec, the frame codec verified against the conformance
fixtures) and contributions are welcome (see below).

```ts
import { Crucible } from "@crucible/sdk";

const cr = new Crucible({ token: process.env.CRUCIBLE_TOKEN });

const sb = await cr.createSandbox({ image: { oci: "python:3.12-alpine" } });
const res = await cr.exec(sb.id!, { cmd: ["python3", "-c", "print(6*7)"] }, {
  onStdout: (b) => process.stdout.write(b),
});
console.log("exit", res.exit_code);

const snap = await cr.snapshot(sb.id!);
const forks = await cr.fork(snap.id!, 8); // warm copies in ~100ms each
```

## Layout

| file | what | how it stays correct |
|---|---|---|
| `src/schema.gen.ts` | **generated** types from the OpenAPI spec | `make gen-ts`, CI drift check |
| `src/types.ts` | named aliases over the schema + `Page<T>` | hand-written |
| `src/frames.ts` | the binary exec-frame codec ([docs/wire.md](../../docs/wire.md)) | conformance fixtures ([`sdks/fixtures`](../fixtures)), `npm test` |
| `src/client.ts` | the fetch client | hand-written, mirrors the Go SDK |
| `src/errors.ts` | `CrucibleError` + `NotFound`/`Unauthorized`/`PolicyDenied` | hand-written |

```sh
npm ci
npm run typecheck   # tsc --noEmit
npm test            # node --test: decodes every conformance fixture
```

## Contributing: good first issues

- **Interactive exec over WebSocket**: the daemon endpoint exists
  (`GET /sandboxes/{id}/exec` + upgrade; contract in
  [docs/wire.md](../../docs/wire.md), reference client in Go at
  `scripts/wsexec`). The frame codec here already encodes stdin frames.
- **`followLogs` helper**: poll `logs()` with the `next_offset` cursor.
- **Tar helper for `putFiles`**: build a tar stream from a directory.
