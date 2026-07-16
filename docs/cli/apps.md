---
title: App commands
description: "The app command family: create and update durable apps, watch their health, read their logs, and exec into the current instance."
---

# App commands

Durable apps are named workloads the daemon keeps a healthy instance of and re-creates from spec after a restart (see [Apps](../apps.md)). `run` is for throwaway work; `app` is for a server you want to stay up.

| Command | Description |
|---|---|
| `create <name> --image <ref> [flags]` | create a durable app; prints its name |
| `update <name> [flags]` | replace the app's spec (same flags as create) and redeploy; **zero-downtime** for a proxy-fronted app (roll the new instance in, flip the route, drain the old); name immutable |
| `ls` | list apps (table: name, desired, phase, health, restarts, instance) |
| `get <name>` | full app JSON (desired state + observed status, incl. `instance_generation`) |
| `rm <name>` | delete the app and tear down its instance |
| `logs <name> [-f] [--source]` | the current instance's durable logs (`-f` reattaches across a redeploy) |
| `exec <name> [-i] [--cwd] [--timeout] [-e] -- <cmd>...` | run a command in the current instance |
| `shell <name> [--shell]` | interactive shell in the current instance |
| `sleep <name>` | snapshot the app and stop its VMM (free RAM+CPU), keeping its identity + ingress route; it wakes **in place** |
| `wake <name>` | wake a slept app (restore in place: same IP, clock stepped to now); idempotent on a running app |

## Create flags

`--image` (required), `--pull`, `--restart always|on-failure|never`, `--health http:PORT[:PATH]|tcp:PORT`, `--health-cmd '<shell command>'` (exec check, exit 0 = healthy), `--port <guest port>` (proxy target), `-p/--publish` (repeatable), `-P/--publish-all` (publish the image's `EXPOSE`d ports), `-e/--env KEY=VALUE` (repeatable, delivered to the entrypoint), `--net-allow` (repeatable), `--net-allow-cidr` (public IPv4 CIDR), `--net-full-egress` (any public host), `--vcpus`, `--memory`, `--disk`, `--stopped`, `--idle-timeout <dur>` (auto-sleep after this much idle time through the ingress proxy; `0`/unset = never), `--min-scale <n>` (`0` enables scale-to-zero; `≥1` keeps that many warm), `--max-scale <n>` (ceiling for horizontal autoscaling; `>` the floor enables it), `--target-concurrency <n>` (autoscaler's in-flight target per instance), `--can-call <app>` (repeatable; grant this app outbound access to `<app>.internal` — needs the daemon's `--internal-networking`), `--internal-port PORT[/tcp|/http]` (repeatable; expose a port to authorized peers at `<app>.internal:PORT` — `tcp` is a raw byte splice incl. TLS passthrough, `http` routes through the L7 proxy — needs the daemon's `--internal-l4`).

## Operate flags

`exec` takes `--cwd`, `--timeout <s>`, `-e/--env KEY=VALUE` (repeatable), and `-i/--interactive`; `shell` takes `--shell <path>` (default `/bin/sh`); `logs` takes `-f/--follow` and `--source service|exec|all`.

```bash
crucible app create web --image nginx:alpine --port 80 -e LOG_LEVEL=info --restart always --health http:80:/
crucible app update web --image nginx:alpine --port 80 --memory 512   # rolls out zero-downtime
crucible app exec web -- /bin/sh -c 'nginx -t'
crucible app logs web -f                                              # reattaches if the app rolls
crucible app sleep web                                                # snapshot + free RAM; route kept
crucible app wake web                                                 # restore in place (same IP)
crucible app create api --image myapi --port 8080 --idle-timeout 5m --min-scale 0  # auto scale-to-zero
crucible app rm web
```

> [!TIP]
> `logs`, `exec`, and `shell` address the app by name; the daemon resolves the current instance **on every call**, so you never look up a sandbox id and they keep working across a self-heal or redeploy. `app logs -f` prints a `== reattached to <id> ==` marker when it follows the app to a new instance.
