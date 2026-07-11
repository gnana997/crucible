---
title: App commands
description: "The app command family: create and update durable apps, watch their health, read their logs, and exec into the current instance."
---

# App commands

Durable apps are named workloads the daemon keeps a healthy instance of and re-creates from spec after a restart (see [Apps](../apps.md)). `run` is for throwaway work; `app` is for a server you want to stay up.

| Command | Description |
|---|---|
| `create <name> --image <ref> [flags]` | create a durable app; prints its name |
| `update <name> [flags]` | replace the app's spec (same flags as create) and redeploy; name immutable |
| `ls` | list apps (table: name, desired, phase, health, restarts, instance) |
| `get <name>` | full app JSON (desired state + observed status) |
| `rm <name>` | delete the app and tear down its instance |
| `logs <name> [-f] [--source]` | the current instance's durable logs |
| `exec <name> [-i] -- <cmd>...` | run a command in the current instance |
| `shell <name>` | interactive shell in the current instance |

## Create flags

`--image` (required), `--pull`, `--restart always|on-failure|never`, `--health http:PORT[:PATH]|tcp:PORT`, `--health-cmd '<shell command>'` (exec check, exit 0 = healthy), `--port <guest port>` (proxy target), `-p/--publish` (repeatable), `-P/--publish-all` (publish the image's `EXPOSE`d ports), `-e/--env KEY=VALUE` (repeatable, delivered to the entrypoint), `--net-allow` (repeatable), `--net-allow-cidr` (public IPv4 CIDR), `--net-full-egress` (any public host), `--vcpus`, `--memory`, `--disk`, `--stopped`.

```bash
crucible app create web --image nginx:alpine -P -e LOG_LEVEL=info --restart always --health http:80:/
crucible app ls
crucible app logs web -f
crucible app rm web
```

> [!TIP]
> `logs`, `exec`, and `shell` resolve the app's current instance automatically; you never need to look up its sandbox id.
