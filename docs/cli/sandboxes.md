---
title: Sandboxes and profiles
description: "The sandbox command family (create, ls, inspect, exec, rm) and profile ls, the daemon's rootfs catalog."
---

# Sandboxes and profiles

## `crucible sandbox`

| Command | Description |
|---|---|
| `create [--vcpus --memory --timeout --profile --image --pull --disk --net-allow -p]` | create a sandbox; prints its id (`--disk 2G` grows the writable rootfs) |
| `ls` | list live sandboxes (table: id, profile, vcpus, mem, net, age) |
| `inspect <id>` | full sandbox JSON |
| `rm <id>...` | destroy one or more sandboxes |
| `exec <id> -- <command>...` | run a command, streaming output; propagates exit code. `--cwd`, `--timeout`, `--env KEY=VALUE` |

```bash
SBX=$(crucible sandbox create --memory 1024 --profile node-22)
crucible sandbox exec $SBX --env NODE_ENV=production -- node -e 'console.log(process.version)'
crucible sandbox rm $SBX
```

> [!TIP]
> Use `--` to separate the guest command from crucible's own flags.

## `crucible profile ls`

Lists the rootfs profiles the daemon was started with (`--rootfs-dir`). These are the values `--profile` accepts. See [Profiles](../profiles.md).
