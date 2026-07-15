---
title: Serverless (wake-on-TCP)
description: "Scale-to-zero for any TCP service: databases, caches, queues, pub/sub. A published port wakes its app on the first connection; it sleeps when idle. Run a serverless postgres or redis that costs zero RAM until someone connects."
---

# Serverless: wake-on-TCP

[Scale-to-zero](apps.md#scale-to-zero) let an idle app sleep and wake on the next
**HTTP request** through the ingress proxy. But most infrastructure (postgres,
mysql, redis, mongo, a message broker, your own daemon) speaks its own protocol
over a raw TCP socket, not HTTP, so it is reached through a **published host
port**, invisible to the L7 proxy. Wake-on-TCP takes scale-to-zero one layer down:
**any** published port is fronted by an L4 waking forwarder that wakes its app on
the first TCP connection and sleeps it again when the app goes idle.

The mechanism is protocol-agnostic (it never parses the bytes), so the same
machinery gives you a **self-hosted serverless database** (postgres, mysql), a
**serverless cache** (redis), or a **scale-to-zero pub/sub / realtime** backend,
all costing zero RAM until someone connects.

```
crucible app create pg --image postgres:16 \
    -p 5432:5432 --volume pgdata:/var/lib/postgresql/data \
    -e POSTGRES_PASSWORD=… \
    --min-scale 0 --idle-timeout 30s --health tcp:5432

psql "host=<daemon-host> port=5432 user=postgres"   # first connect wakes it
# … use it, disconnect …
#  idle: the app sleeps, freeing VM + RAM and detaching the volume
psql …                                              # reconnect wakes it, data intact
```

## How it works

An app that publishes a host port and is scale-to-zero (`--min-scale 0
--idle-timeout <dur>`) gets an **app-scoped** forwarder that owns the host port
for the app's whole life, independent of any one instance, so it survives the
app's sleep. On each incoming connection it:

1. resolves the app's **current** instance (fresh, by name);
2. if the app is asleep, **wakes it**, holding the connection until it is running
   (a burst of connections coalesces into a single wake);
3. dials the now-running guest and splices bytes both ways.

Because it resolves per connection, it re-targets a **new instance** after a wake
that cold-creates one (a volume app), and a same-address restore (a non-volume
app) just works. No client data is read before the backend is dialed, so the bytes
a client sends on connect (a postgres startup packet) are never lost.

## Two modes: request/response vs connection-scoped

The one thing that varies by workload is **when a connection counts as idle**, and
that determines when the app can sleep. An app sleeps only when it has zero open
connections, so the question is whether a *held-but-quiet* connection should be
reaped.

### Request/response (the default): reaping on

Databases and caches with a **connection pool** hold connections open between
queries. Left untouched, one pooled connection would keep the app awake forever.
So by default the forwarder **reaps a connection after it has been byte-idle for
`--connection-idle-timeout`** (which defaults to `--idle-timeout`). The pooled
connection is closed; the client's pool re-establishes it on its next query (the
standard model for serverless databases); when the client is truly done,
connections drain to zero and the app sleeps. A bursty-but-active client keeps its connection alive and the app
awake, correctly, because it is not idle.

This is the mode for **postgres, mysql, redis GET/SET, mongo**: anything
request/response.

### Connection-scoped (`--keep-connections`): reaping off

A **subscription** is a long-lived connection that is *quiet but alive*: a redis
`SUBSCRIBE`, a postgres `LISTEN/NOTIFY`, an MQTT session, a websocket stream.
Reaping it would drop the subscription, and sleeping the VM under it would lose
messages. So `--keep-connections` turns reaping **off**: the forwarder never closes
a connection on silence (only TCP keepalive reaps a genuinely dead peer), and the
app stays awake as long as any client is connected, sleeping only when the **last
one disconnects** and waking when the first reconnects.

This is the mode for **pub/sub brokers, realtime/websocket gateways, LISTEN/NOTIFY**:
connection-scoped workloads where "idle" means "nobody is connected," not "no recent
bytes."

## Configuration

Wake-on-TCP activates automatically when an app is **scale-to-zero**
(`--min-scale 0 --idle-timeout <dur>`) and **publishes an explicit host port**
(`-p HOST:GUEST`). No daemon flag, no ingress proxy required.

| Flag | Effect |
|---|---|
| `--min-scale 0 --idle-timeout <dur>` | scale-to-zero: sleep after `<dur>` with zero connections |
| `-p HOST:GUEST` | the published port the L4 forwarder fronts (repeatable) |
| `--connection-idle-timeout <dur>` | reap a connection idle this long (default is `--idle-timeout`) |
| `--keep-connections` | never reap on silence; sleep only when clients disconnect (pub/sub) |
| `--health tcp:<port>` | wait for the service to accept connections before routing |
| `--volume NAME:/path` | durable storage for a database |

`-P` / `--publish-all` (image `EXPOSE`) is **not** a wake trigger: the forwarder
needs an explicit host-to-guest mapping, so pair a serverless app with `-p`. A
scale-to-zero app with `--port` (an HTTP proxy route) instead of `-p` wakes through
the proxy; one with neither is rejected (nothing could wake it).

## Wake latency

| App | Sleep mode | Wake |
|---|---|---|
| **Non-volume** (stateless cache, ephemeral redis) | snapshot | **~125-170 ms**: restore in place, same IP |
| **Volume-backed** (postgres, redis with persistence) | snapshot | **~170 ms (reflink) – ~240 ms (ext4)**: restore in place, same IP, volume re-attached |

Both wake by snapshot restore, so a serverless **postgres** comes back **without a
cold boot or WAL recovery**: the database process is already running in the
restored memory, attached to its volume, in about 170 ms on a reflink filesystem
(btrfs / XFS) and ~240 ms on ext4 — the volume adds no meaningful overhead over a
stateless wake. A same-lifetime wake is in place (same instance and IP); a wake
after a **daemon restart** restores a fresh instance from the durable snapshot
(new IP, still no cold boot), with the volume re-attached and data intact. If a
restore ever fails, the app falls back to a cold boot so a wake never fails; it
just isn't instant that once. See [benchmarks.md](benchmarks.md#stateful-volume-wake--v062)
for the measured distribution.

## What fits and what doesn't

**Fits well:** mostly-idle infrastructure where connections come and go.

- request/response databases and caches with pooled clients (postgres, mysql,
  redis, mongo); reaping lets them scale to zero between bursts of use;
- dev / preview / CI environments, per-tenant databases, internal tools, agent
  sandboxes; idle most of the time, so they mostly cost nothing;
- connection-scoped pub/sub / realtime backends with `--keep-connections`; awake
  while clients are connected, asleep overnight.

**Doesn't fit** (inherent to scale-to-zero, not a limitation we can remove):

- a service that cannot tolerate the ~170 ms snapshot-restore pause on the first
  request after it has gone idle (that wake is fast, but not zero);
- durable message delivery to a *disconnected* subscriber; a slept pub/sub app is
  not holding the connection, so fire-and-forget messages published while it is
  asleep have no one to deliver to (persistent queues that keep messages on a
  volume are fine, since the messages survive the sleep and replay on wake);
- a workload that holds a connection open *and* sends traffic constantly; it is
  never idle, so it never sleeps (which is correct).

## Notes

- **Postgres on a volume, two gotchas.** Set `PGDATA` to a **subdirectory** of the
  mount (`-e PGDATA=/var/lib/postgresql/data/pgdata`) so the volume's `lost+found`
  does not make the entrypoint think the data dir is already initialized.
  Password-authenticated init works out of the box, because the guest provides
  `/dev/fd`, which the entrypoint's process-substitution password file needs.
- **Reachability.** A published port needs the daemon's `--network-egress-iface`
  set, as any host port publish does.

Acceptance: `scripts/smoke_serverless.sh` covers nginx (arbitrary TCP), a
password-authenticated serverless postgres, a request/response redis (reaping), and
a `--keep-connections` pub/sub redis. See also [apps.md](apps.md),
[volumes.md](volumes.md).
