---
title: Snapshots and fork
description: "Freeze a warm sandbox with snapshot, then fork N independent copies of it in milliseconds: the workflow crucible is built around."
---

# Snapshots and fork

Snapshot a sandbox after expensive setup (clone, install, warm caches), then fork as many independent copies as you need. Each fork resumes from the frozen state in milliseconds instead of repeating the setup.

## `crucible snapshot`

| Command | Description |
|---|---|
| `create <sandbox-id>` | snapshot a sandbox; prints the snapshot id |
| `ls` | list snapshots (table: id, source, vcpus, mem, age) |
| `inspect <id>` | full snapshot JSON |
| `rm <id>...` | delete snapshots |

## `crucible fork`

`fork <snapshot-id> [--count N] [-p HOST:GUEST]` creates `N` sandboxes (default 1) from a snapshot and prints the new sandbox ids. Each child is fully independent: its own network and, via clone-safety, its own RNG and machine identity.

`-p/--publish` maps a host port onto the fork (`[HOST_IP:]HOST:GUEST[/tcp]`, same as `run -p`): fork a running server and expose the copy.

> [!NOTE]
> Publishing requires `--count 1`. Host ports are exclusive, so a fan-out cannot share them.

```bash
SBX=$(crucible sandbox create --profile python-3.12)
crucible sandbox exec $SBX -- pip install -r requirements.txt
SNP=$(crucible snapshot create $SBX)
crucible fork $SNP --count 5            # five warm, independent copies
crucible fork $SNP -p 8081:80           # one fork, reachable on host port 8081
```
