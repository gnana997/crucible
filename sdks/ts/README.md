# crucible TypeScript types

`src/schema.gen.ts` is **generated — do not edit by hand.** It is produced
from [`docs/openapi.json`](../../docs/openapi.json) (itself reflected from
the Go wire types, so the chain Go → spec → TS cannot drift) by:

```sh
make gen-ts   # openapi-typescript, version-pinned in the Makefile
```

CI regenerates it and fails on any diff, mirroring the spec's route-coverage
test.

The hand-written TypeScript SDK (fetch-based client + the binary exec-frame
codec, which OpenAPI cannot model — see [`docs/api.md`](../../docs/api.md)
for the frame format) will live alongside these types. Contributions
welcome once the scaffold lands.
