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

In the daemon it's **off by default**. Enable it with a listen address and a base
domain:

```bash
crucible daemon … --proxy-listen :8080 --proxy-tls-listen :8443 --proxy-domain apps.local
```

```bash
crucible app create web --image nginx:alpine --port 80
curl -H 'Host: web.apps.local' http://<daemon-host>:8080/
```

**The installer turns it on for you.** `install.sh` seeds these flags into the
daemon config by default, on the high ports **`:8080`/`:8443`** — deliberately
*not* `:80`/`:443`, so those stay free for direct port-publishing (`run -p 80:80`,
`-P`) and so a pre-existing web server on `:80` can't block daemon start (a busy
proxy port aborts the daemon). Override at install with
`PROXY_LISTEN=`/`PROXY_TLS_LISTEN=`/`PROXY_DOMAIN=`, or `--no-proxy` to skip it.
Any free TCP ports work; `host:port` pins an interface (`127.0.0.1:8080` =
loopback only).

For a **production ingress** on the standard ports, use `--proxy-listen :80
--proxy-tls-listen :443` (`PROXY_LISTEN=:80 PROXY_TLS_LISTEN=:443` at install) —
the daemon runs as root under systemd, so it binds them without extra caps, and
then apps are reachable at plain `http://web.apps.local/`.

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
| `--proxy-listen` (e.g. `:8080`) | L7 (HTTP) | Routes by the `Host` header, reverse-proxies to the instance (keep-alive, chunked, `X-Forwarded-*`). |
| `--proxy-tls-listen` (e.g. `:8443`) | L4 (TLS SNI) | Reads the TLS ClientHello's SNI and **passes the raw stream through** to the instance — **the guest terminates its own TLS**. The proxy holds no certificates. |

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
the proxy. The one caveat is the obvious one: a host port has exactly one owner,
so the proxy and a published port can't share the same number. This is why the
installer defaults the proxy to `:8080`/`:8443` — it leaves `:80`/`:443` free to
`-p 80:80` or `-P`. Put the proxy on `:80` and publishing *to* host `:80` is no
longer available (the daemon can't bind it twice); that's expected — on a
proxy-fronted host you reach apps by name instead of publishing them to `:80`.

## Model

The proxy runs **in-process** in the daemon (mirroring the DNS proxy), needs
durable apps (`--app-db`), and binds whatever ports you give it — the high
defaults (`:8080`/`:8443`) need no privileges, and `:80`/`:443` are bindable
because the daemon runs as root under the systemd unit. See
[architecture.md](architecture.md) for where it sits and [apps.md](apps.md) for
the app model it routes to.
