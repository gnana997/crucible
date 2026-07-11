---
title: Sandbox lifecycle
description: "Stop a workload without losing the box, tear one down, open a live shell, and copy files in: stop, rm, shell, and cp."
---

# Sandbox lifecycle

## `crucible stop` and `crucible rm`

`stop <id>...` gracefully stops a sandbox's entrypoint (image StopSignal, a grace period, then SIGKILL) while keeping the sandbox: the ops "pull the plug on the workload" action. `rm <id>...` (alias `delete`) removes the sandbox with a hard kill, the same as `sandbox rm`. Both are top-level for docker-parity muscle memory.

```bash
crucible stop sbx_abc      # halt the workload, keep the box
crucible rm sbx_abc        # tear the box down
```

## `crucible shell`

`shell <id>` opens a live interactive shell into a running sandbox. `cd`, environment, and shell state persist across commands within the session. It is line-oriented (no PTY), and the fast way to poke at untrusted code you just booted.

```bash
SBX=$(crucible sandbox create --profile python-3.12)
crucible shell $SBX        # a real /bin/sh inside it; `exit` to leave
```

Override the shell with `--shell /bin/bash`.

## `crucible cp`

`cp <src> <dst>` copies a local file or directory into a running sandbox (host to guest): drop code in and run it, no image build, no Dockerfile. Directories are recursive; the destination is treated as a directory and the source's basename is preserved under it (`cp ./app sbx:/work` lands at `/work/app`). Parents are created and existing files overwritten.

```bash
SBX=$(crucible run --profile python-3.12)
crucible cp ./script.py $SBX:/work                 # -> /work/script.py
crucible sandbox exec $SBX -- python /work/script.py
```

A `sbx_...:<path>` operand is the sandbox side; the other is the local path.

> [!NOTE]
> Copying out of a sandbox is exposed to agents through the MCP `read_file` tool (see [MCP](../mcp.md)); a CLI pull lands in a later release.
