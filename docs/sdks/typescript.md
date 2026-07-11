---
title: TypeScript SDK
description: "A typed, zero-runtime-dependency fetch client with streaming exec and a frame codec verified against the shared conformance fixtures."
---

# TypeScript SDK

A typed, zero-runtime-dependency `fetch` client. **Scaffold status**: the
core works (typed client, streaming exec, a frame codec verified against
the conformance fixtures), and it is **not yet published to npm**. Until
then, consume it from the repo (`sdks/ts`, source-first: Node ≥ 22.18
runs it directly) or vendor the `src/` files.

Server-side runtimes only (Node, Bun, Deno): a daemon token grants
control of the host's microVMs and must never ship to a browser.

```ts
import { Crucible, NotFoundError } from "@crucible/sdk";

const cr = new Crucible({
  baseUrl: "http://127.0.0.1:7878",
  token: process.env.CRUCIBLE_TOKEN,
});

// Boot from an OCI image, run a command, stream its output:
const sb = await cr.createSandbox({ image: { oci: "python:3.12-alpine" } });
try {
  const res = await cr.exec(sb.id!, { cmd: ["python3", "-c", "print(6*7)"] }, {
    onStdout: (chunk) => process.stdout.write(chunk),
    onStderr: (chunk) => process.stderr.write(chunk),
  });
  console.log("exit", res.exit_code, "peak mem", res.usage?.peak_memory_bytes);

  // Snapshot the warm state, fork copies (~100ms each):
  const snap = await cr.snapshot(sb.id!);
  const forks = await cr.fork(snap.id!, 8);
  await Promise.all(forks.map((f) =>
    cr.exec(f.id!, { cmd: ["python3", "attempt.py"] }).finally(() => cr.deleteSandbox(f.id!)),
  ));
} finally {
  await cr.deleteSandbox(sb.id!);
}
```

## What's covered

Sandboxes (create/list/get/delete), streaming `exec` with stdout/stderr
callbacks, snapshots + `fork`, images (pull/import/list/delete), durable
`logs`, file push (`putFiles`, tar stream) and `readFile`, the supervised
service API, typed errors (`NotFoundError` / `UnauthorizedError` /
`PolicyDeniedError` over `CrucibleError`), and `Page<T>` list shapes,
mirroring the [Go SDK](go.md) surface.

Types are **generated from the OpenAPI spec** (`schema.gen.ts`, with
readable aliases in `types.ts`), so they cannot drift from the daemon.
The frame codec (`frames.ts`) passes the full
[conformance suite](../wire.md) against the recorded fixtures; `npm
test` needs no daemon and no KVM.

## Not yet built (contributions welcome)

- **Interactive exec**: the daemon's WebSocket transport
  (`GET /sandboxes/{id}/exec` + upgrade) exists and is smoke-tested; the
  codec here already encodes stdin frames. Contract in the
  [wire doc](../wire.md), reference client in Go at `scripts/wsexec`.
- **Log following**: poll `logs()` with the `next_offset` cursor.
- **Tar helper**: build a `putFiles` stream from a local directory.
- **npm packaging**: build output, package name, publish provenance.

See [`sdks/ts/README.md`](https://github.com/gnana997/crucible/tree/main/sdks/ts)
for the layout and how each file stays correct.
