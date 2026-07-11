---
title: Ingress proxy
description: "One daemon-owned listener that routes requests to many apps by name: the front door when one host runs more than one app."
---

# Ingress proxy: reach an app by name

A published port (`-p`) gives one app one host port. The **ingress proxy** is the
front door for *many* apps on one host: a daemon-owned listener that routes
inbound traffic to an app's **current instance** by name — so you address `web`,
not `sbx_9f2ac1:8080`, and the route follows the app across self-heal and
redeploys.

It's **off by default**. Enable it with a listen address and a base domain:

```bash
crucible daemon … --proxy-listen :80 --proxy-tls-listen :443 --proxy-domain apps.local
```

```bash
crucible app create web --image nginx:alpine --port 80
curl -H 'Host: web.apps.local' http://<daemon-host>/
```

## How it routes

- **`--proxy-domain apps.local`** — an app named `web` is reached at
  `web.apps.local`. The proxy strips the domain suffix to get the app name, then
  resolves that app's current instance. (Empty `--proxy-domain` means the request
  `Host` *is* the app name.)
- **`--port`** — the guest port the proxy forwards to. Defaults from a single
  published/`EXPOSE`d port; set it explicitly when the image exposes several.
- **Resolution is live** (with a ~1s cache), so it never routes to a stale IP: an
  instance's guest IP changes on every re-create/fork, and the app object is the
  source of truth for which instance is current.

## Two listeners

| Listener | Layer | What it does |
|---|---|---|
| `--proxy-listen` (`:80`) | L7 (HTTP) | Routes by the `Host` header, reverse-proxies to the instance (keep-alive, chunked, `X-Forwarded-*`). |
| `--proxy-tls-listen` (`:443`) | L4 (TLS SNI) | Reads the TLS ClientHello's SNI and **passes the raw stream through** to the instance — **the guest terminates its own TLS**. The proxy holds no certificates. |

TLS *termination* at the proxy (with ACME / custom domains) is later work; today
the guest owns its cert and the proxy just routes by SNI.

## When there's no instance

- **Unknown host / app** → HTTP `404` (or the TLS connection is closed).
- **Known app, no ready instance** (booting, crash-looping, stopped) → HTTP `502`.
  There is no request buffering or wake — that arrives with sleep/wake.

## Isolation

Inbound reaches a guest **only** from the proxy (and any published ports), which
dial the instance from the daemon's host network namespace. Guests still cannot
reach each other or the host's private ranges — the introduction of inbound
traffic does not weaken the per-sandbox isolation described in
[network.md](network.md). A guest cannot reach the proxy itself either: the host
input chain drops every guest-initiated packet to a host-local address, so even
a `--net-full-egress` guest can't pivot through the proxy to another app —
published or not.

## Coexists with `-p`

Publishing a host port with `-p`/`--publish` still works and is the raw-TCP
bypass path — an app can be reached both by a published port and by name through
the proxy.

## Model

The proxy runs **in-process** in the daemon (mirroring the DNS proxy), needs
durable apps (`--app-db`), and binds `:80`/`:443` as root under the systemd unit.
See [architecture.md](architecture.md) for where it sits and
[apps.md](apps.md) for the app model it routes to.
