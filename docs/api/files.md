---
title: File transfer
description: "Push a tar stream into the guest or read one file's bytes out: the two file endpoints behind crucible cp and the MCP file tools."
openapi: "POST /sandboxes/{id}/files"
---

File transfer between host and guest is one-way bulk copy, so there is no frame protocol.

## Push

`POST /sandboxes/{id}/files?path=<dest>` takes a tar stream as the request body. The daemon streams it straight to the guest agent (nothing buffered whole); the agent creates `<dest>` and extracts each entry beneath it, rejecting any entry whose resolved path escapes the destination (absolute paths, `..`, or symlinks pointing outside). Returns `200 {"files":N,"bytes":M}`. Gated as an `exec`-class operation.

This backs `crucible cp` (push) and the MCP `write_files` tool.

## Read

`GET /sandboxes/{id}/files?path=<file>&max_bytes=<n>` returns the raw bytes of a single guest file, capped by `max_bytes`; a directory is a `400`. Gated as `read`. It backs the MCP `read_file` tool.

> [!NOTE]
> The read side is a content read: only bytes flow out and nothing is written host-side, so it has no path-traversal surface. Pulling a directory tree onto the host is intentionally not offered.
