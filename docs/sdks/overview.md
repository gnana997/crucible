# SDKs

Drive crucible from your own code: boot microVM sandboxes, stream `exec`
output, push files in, snapshot a warm state and fork copies in ~100&nbsp;ms.
These are the primitives agentic products are built on, self-hosted on your
own hardware.

| SDK | Status | Where |
|---|---|---|
| **Go** | ✅ Stable surface, `sdk/v0.2.0` | [`github.com/gnana997/crucible/sdk`](https://pkg.go.dev/github.com/gnana997/crucible/sdk) · [guide](go.md) |
| **TypeScript** | 🧪 Scaffold: core client + frame codec work; not yet on npm | [`sdks/ts`](https://github.com/gnana997/crucible/tree/main/sdks/ts) · [guide](typescript.md) |
| **Python** | 🙋 Help wanted: generated Pydantic models exist, client layer open | [`sdks/python`](https://github.com/gnana997/crucible/tree/main/sdks/python) · [details](python.md) |
| Anything else | The API is plain HTTP + one documented binary stream | [API reference](/api-reference), [wire protocol](../wire.md) |

## One contract, N languages

Every SDK is the same thin thing: a typed HTTP client over the daemon's
REST API, plus a small binary-frame codec for the exec stream. Nothing
else: auth decisions, policy, and all sandbox logic live in the daemon.
Three artifacts keep every language in lock-step:

1. **[`openapi.json`](/api-reference)**, generated from the daemon's Go
   wire types with a coverage test, so the spec cannot drift from the
   code. TypeScript and Python types are generated *from the spec*
   (`make gen`), and CI fails if any generated artifact is stale.
2. **[The wire protocol spec](../wire.md)**, the one thing OpenAPI can't
   express: the exec frame stream and its two interactive transports
   (hijacked connection and WebSocket), specified byte-by-byte.
3. **Conformance fixtures**
   ([`sdks/fixtures`](https://github.com/gnana997/crucible/tree/main/sdks/fixtures)):
   recorded frame streams + a manifest describing every frame. An SDK's
   codec is testable in any language **with no daemon and no KVM**: decode
   the files, compare against the manifest. The fixtures are generated
   from the reference codec itself, so they can't disagree with the
   implementation.

## Trust model

A daemon API key grants control of that host's microVMs. SDKs are
**server-side libraries**: your backend holds the key and creates
sandboxes on behalf of your users. The same way you'd never expose your
database to browsers, never ship a crucible token to one. See
[authentication](../api.md#authentication) and
[policy](../policy.md) for scoped tokens.
