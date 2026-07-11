---
title: Fork
description: "POST /snapshots/{id}/fork: fan out N independent sandboxes from one snapshot, all-or-nothing, with optional port publishing on a single fork."
openapi: "POST /snapshots/{id}/fork"
---

Create sandboxes from a snapshot. Fan-out is set by the `?count=N` query parameter (default `1`); `N` must be a positive integer and is capped by the daemon's max fork count (`400` if exceeded).

An optional JSON body (`{"count", "publish"}`) can set the count and publish host ports on the fork (`docker run -p` semantics: fork a running server onto its own port). The body's `count` wins over the query param; `publish` requires `count == 1`, because host ports are exclusive. A body-less request keeps the query-only form working on any daemon.

> [!IMPORTANT]
> Fork is all-or-nothing: if any child fails to come up, every child started so far is rolled back and the call returns an error.

On success, returns `201`:

```json
{ "sandboxes": [ ... ] }
```

Each child is a fully independent sandbox: its own ID, its own network, and, thanks to clone-safety, its own fresh RNG state and machine identifiers. Errors: `400` (bad `count` or invalid config), `404` (unknown snapshot), `500`.
