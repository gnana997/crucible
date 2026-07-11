# crucible Python types

`crucible/models.py` is **generated — do not edit by hand.** It contains
Pydantic v2 models produced from
[`docs/openapi.json`](../../docs/openapi.json) (itself reflected from the Go
wire types, so the chain Go → spec → Python cannot drift) by:

```sh
make gen-py   # datamodel-code-generator, version-pinned in the Makefile
```

CI regenerates it and fails on any diff, mirroring the spec's route-coverage
test.

The hand-written Python SDK (httpx-based client + the binary exec-frame
codec, which OpenAPI cannot model — specified in
[`docs/wire.md`](../../docs/wire.md), with conformance fixtures under
[`sdks/fixtures`](../fixtures)) will live alongside these models. Contributions
welcome once the scaffold lands.
