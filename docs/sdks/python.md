---
title: Python SDK
description: "Help wanted: no Python client exists yet, and it is a deliberately well-prepared contribution with the hard parts already specified and fixtured."
---

# Python SDK: help wanted

There is no Python client yet, and it is a deliberately well-prepared
contribution: **almost everything hard already exists, tested**, and what's
left is a thin, pleasant httpx layer. If you like clean protocol work,
this is a great way in. No KVM, no daemon, no Firecracker needed to
develop or test the core.

## What you build on (not from scratch)

- **Generated Pydantic v2 models**:
  [`sdks/python/crucible/models.py`](https://github.com/gnana997/crucible/tree/main/sdks/python),
  produced from the OpenAPI spec by `make gen-py` and drift-checked in
  CI. The request/response types are done; don't hand-write them.
- **The wire spec**: [wire.md](../wire.md) specifies the one binary part
  (the exec frame stream) byte-by-byte, with a four-step test recipe.
- **Conformance fixtures**:
  [`sdks/fixtures`](https://github.com/gnana997/crucible/tree/main/sdks/fixtures)
  holds recorded frame streams + a manifest describing every frame (types,
  lengths, SHA-256s, parsed exit results, invalid streams with required
  failure modes). Your codec tests decode these files and compare;
  that's the whole test story.
- **Two reference implementations**: Go
  ([`sdk/wire`](https://github.com/gnana997/crucible/tree/main/sdk/wire) +
  [`sdk`](https://github.com/gnana997/crucible/tree/main/sdk)) and
  TypeScript ([`sdks/ts`](https://github.com/gnana997/crucible/tree/main/sdks/ts),
  whose `test/frames.test.ts` shows exactly how to drive the fixtures).

## Scope of a first PR

1. `frames.py`: encode/decode per the wire spec (~100 lines; mirror
   `frames.ts`) + a pytest walking the fixture manifest.
2. `client.py`: httpx-based (sync first), mirroring the
   [TypeScript surface](typescript.md): sandboxes, streaming `exec` with
   callbacks, snapshots/fork, images, logs, files, services; typed errors
   and `Page` lists matching the other SDKs.
3. Dependency-light: `httpx` + `pydantic` only.

Interactive exec (WebSocket), tar helpers, and PyPI packaging are
explicit follow-ups, also welcome.

If the wire doc or fixtures are ever ambiguous, that's a bug in **our
spec**, not your code: report it and the spec gets fixed. Open an issue
or PR at [github.com/gnana997/crucible](https://github.com/gnana997/crucible).
