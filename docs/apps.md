---
title: Apps
description: "Durable, self-healing workloads: a named spec the daemon keeps a healthy instance of, health checks, restart policies, and recovery after a daemon restart."
---

# Apps: durable, self-healing workloads

A **sandbox** is ephemeral — you spin it up, run something, tear it down, and a
daemon restart drops it. An **app** is the opposite: a named workload the daemon
*manages over time*. It keeps a healthy instance running, restarts it on
failure, health-checks it, and — the headline — **re-creates it from spec after
a daemon restart or host reboot**.

Use `crucible run` for throwaway work; use `crucible app` for a server you want
to stay up.

```bash
crucible app create web --image nginx:alpine -p 8080:80 \
  --restart always --health http:80:/
crucible app ls
# NAME  DESIRED  PHASE    HEALTH   RESTARTS  INSTANCE
# web   running  running  healthy  0         sbx_9f2ac1
```

## What an app is

An app is **desired state** the daemon converges toward. It owns at most one
running **instance** at a time — an ordinary sandbox, booted from the app's
image with its published ports, network policy, and entrypoint. The app's
`name` is a stable handle; its instance id changes each time the instance is
(re-)created.

Desired state is persisted in a small control-plane store (separate from the
ephemeral sandbox registry), so it outlives the daemon.

## Survives a restart

This is the point of apps. When the daemon restarts (an upgrade, a crash, a host
reboot), the old instances are gone — but each app's spec is still in the store,
so the reconciler **boots a fresh instance from it**. Your app comes back.

It is *re-created*, not live-re-attached: the new instance is a cold boot from
the image (~a couple of seconds), and in-VM memory from before the restart is
gone. That is exactly right for a stateless server (nginx, an API, a worker) —
the 95% case. (Surviving *with* in-VM memory intact is later trajectory work.)

```bash
crucible app create web --image nginx:alpine -p 8080:80 --restart always
curl localhost:8080          # served by the instance
sudo systemctl restart crucible
curl localhost:8080          # served again — a fresh instance, re-created from spec
```

Forks stay ephemeral by design: fork fan-out is short-lived exploration, so
surviving a restart is irrelevant to it.

## Self-healing

The daemon keeps the app healthy along two axes.

**Restart policy** governs what happens when the *instance* dies:

| Policy | Behavior |
|---|---|
| `always` (default) | restart on any exit |
| `on-failure` | restart on a non-clean exit |
| `never` | leave it stopped |

Restarts use **exponential backoff** (1s, doubling, capped at 60s) so a broken
instance isn't hot-looped, and a **crash-loop guard**: after several fast
failures the app enters a `crashlooping` phase (surfaced in status) and is
retried at the capped interval — the same shape as Kubernetes' CrashLoopBackOff.
An instance that runs healthy past a window resets the failure count, so a
one-off crash hours later restarts normally rather than counting as a loop.

> Two restart levels, don't confuse them: the **guest supervisor** restarts a
> crashed *process inside a live instance*; the **daemon reconciler** (this) boots
> a replacement when the *whole instance* is gone or unhealthy.

**Health checks** are the liveness signal the daemon probes:

```bash
--health http:80:/                    # GET / on guest port 80, expect 2xx
--health tcp:5432                      # TCP connect to guest port 5432 succeeds
--health-cmd 'pg_isready -U postgres'  # run a command in the guest, exit 0 = healthy
```

An instance that fails its health check past the threshold is destroyed and
restarted (subject to the backoff above). A start-period grace window means slow
starters aren't killed while warming up. Without a health check, "process alive"
is the liveness signal. The `exec` check (`--health-cmd`) runs its command in the
guest over vsock, so it works even for an app with no network. An app that
declares no health of its own **inherits the image's Docker `HEALTHCHECK`** when
it has one (seeded as an `exec` check at first boot and persisted); pass
`--health`/`--health-cmd` to override.

## The `crucible app` commands

| Command | What |
|---|---|
| `app create <name> --image <ref> [flags]` | create a durable app; prints its name |
| `app update <name> [flags]` | replace the app's spec and redeploy; **zero-downtime** for a proxy-fronted app (see below); name immutable |
| `app ls` | list apps with desired state, phase, health, restarts, instance |
| `app get <name>` | full desired state + observed status (JSON) |
| `app rm <name>` | delete the app and tear down its instance |
| `app logs <name> [-f] [--source]` | the instance's durable logs |
| `app exec <name> [-i] -- <cmd>` | run a command in the current instance |
| `app shell <name>` | interactive shell in the current instance |
| `app sleep <name>` | snapshot + stop the VMM (free RAM+CPU), keeping identity + route; wakes **in place** |
| `app wake <name>` | wake a slept app (restore in place: same IP, clock stepped to now) |

`create` flags: `--image` (required), `--pull`, `--restart`, `--health`
(http/tcp), `--health-cmd` (exec), `--port` (proxy target port), `-p/--publish` (repeatable),
`-P/--publish-all` (publish every port the image `EXPOSE`s, guest N → host N),
`-e/--env KEY=VALUE` (repeatable), `--net-allow` (repeatable),
`--net-allow-cidr` (public IPv4 CIDR, repeatable), `--net-full-egress` (reach any
public host), `--vcpus`, `--memory`, `--disk`, `--stopped` (create without
starting an instance), `--idle-timeout <dur>` + `--min-scale <n>` (scale to zero
— see below).

Env vars are delivered to the app's entrypoint (image `ENV` < your `--env`, so
yours win); `-P` reads the ports the image declares, so `crucible app create web
--image nginx:alpine -P` publishes :80 without a manual `-p`.

`logs`/`exec`/`shell` resolve the app's current instance **on every call** (the
daemon does it server-side), so you never juggle the instance id and they keep
working across a self-heal or redeploy. `app logs -f` reattaches to the new
instance when the app rolls (a `== reattached to <id> ==` marker). `app exec`
takes `--cwd`/`--timeout`/`-e,--env`; `app shell` takes `--shell`.

## Zero-downtime update

For a **proxy-fronted app** (an app with a `--port` and no fixed host publish),
`app update` rolls the new spec out without dropping traffic:

1. Boot the new instance **without** flipping to it — the old instance keeps
   serving.
2. Wait for the new instance to pass its **readiness gate**: its health check if
   it has one, otherwise a TCP connect to the app's `--port`.
3. **Flip the ingress route** to the new instance (the proxy follows it within
   its ~1s resolution TTL), then keep the old instance alive for a short **drain
   window** so in-flight requests finish, and finally destroy it.

If the new instance never becomes ready within the rollout deadline (or
crash-loops), the update **aborts and the old instance keeps serving** —
`app get` shows the failure in `last_error` and `instance_generation` stays on
the old spec. A bad update never takes the app down. Apps that publish a fixed
host port (or have no `--port`) can't run two instances at once, so they keep the
simpler destroy-then-boot redeploy.

## Scale to zero

An app can **sleep when idle and wake on the next request in under a second**.
Sleeping snapshots the running guest and stops its VMM — freeing its RAM and CPU
— while **keeping** the netns, subnet/IP reservation, and ingress route, so the
app stays addressable at ~zero cost. Waking restores it **in place**: the same
instance id and IP (no DHCP bounce, no proxy re-resolution), with the guest CRNG
reseeded and its clock stepped to the current time *before it serves* — but,
unlike a fork, machine-id and hostname are **not** rotated. A wake is
snapshot-restore with lazy (`userfaultfd`) memory, so it costs the working set,
not the whole guest RAM.

**Manual:** `crucible app sleep web` / `crucible app wake web`.

**Automatic:** `app create --idle-timeout <dur> --min-scale 0`. The ingress proxy
tracks each app's last-activity time and open-connection count; once the app has
been idle for `--idle-timeout` **and** has no open connections **and** is
healthy, the reconciler sleeps it. The next request through the proxy **triggers
a wake, holds the request, and forwards it when the app passes its readiness
probe** — a herd of requests hitting one sleeping app coalesces into a **single**
wake, and a wake that can't be served in time gets a clean `503`. `--min-scale
≥1` keeps that many instances always-warm (today's default behavior);
`--idle-timeout 0` never sleeps.

**Durability & guards.** Sleep captures a **durable** snapshot (journaled record
+ cloned rootfs), so a slept app survives a daemon restart — it's re-adopted on
start, and the first post-restart request wakes a fresh instance from the
snapshot. A wake is refused (the request gets a `503`, the app stays asleep) when
host free memory is below `--wake-min-free-mib` (daemon flag, default 256) rather
than thrashing the box. Sleeping drains in-flight requests; idle keepalive TCP
connections are reset at sleep, and the proxy never reuses a pre-sleep upstream
connection.

## Status fields

`app get` / `app ls` surface the observed status the reconciler maintains:

- **phase** — `pending` (booting / backing off), `running`, `crashlooping`,
  `stopped`, `asleep` (snapshotted, VMM stopped), `waking` (restoring on a request)
- **health** — `healthy`, `unhealthy`, `unknown` (no check, or in the start period)
- **restarts** — how many times the daemon has restarted the instance
- **instance_id** — the sandbox currently backing the app (empty when none)
- **instance_generation** — the spec generation the live instance was booted
  from; it lags `generation` while a rolling update is in progress or after a
  failed update (the old instance is still serving the previous spec)
- **last_wake_latency_ms** / **sleep_count** — for a scale-to-zero app, the most
  recent wake's request→served latency and how many times it has slept

## From the API / SDKs

Apps are the REST `/apps` routes (see [api.md](api.md)) and are first-class in
the Go SDK:

```go
cr.CreateApp(ctx, api.CreateAppRequest{AppSpec: api.AppSpec{
    Name:    "web",
    Image:   &api.ImageRef{OCI: "nginx:alpine"},
    Publish: []api.PortMapping{{HostPort: 8080, GuestPort: 80}},
    Restart: wire.RestartPolicy{Policy: wire.RestartAlways},
    Health:  &api.HealthCheck{Type: "http", Path: "/", Port: 80},
}})

app := cr.App("web")
res, _ := app.Exec(ctx, wire.ExecRequest{Cmd: []string{"nginx", "-t"}}, os.Stdout, os.Stderr)

app.Sleep(ctx) // snapshot + free RAM, keep the identity + route
app.Wake(ctx)  // restore in place (same IP, clock stepped to now)
```

Reach an app by name through the [ingress proxy](proxy.md) instead of juggling
published ports.

MCP agents get `create_app` / `update_app` / `list_apps` / `get_app` /
`delete_app` tools (see [mcp.md](mcp.md)), under the same operator guardrails as
sandbox creation.
