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
crucible daemon … --proxy-listen :7879 --proxy-domain apps.local
```

```bash
crucible app create web --image nginx:alpine --port 80
curl -H 'Host: web.apps.local' http://<daemon-host>:7879/
```

**The installer turns it on for you.** `install.sh` seeds these flags into the
daemon config by default, on **`:7879`** — right next to the `:7878` API and
deliberately *out of the ports you publish apps to* (`:80`, `:443`, `:8080`,
`:3000`, `:8000`). That matters: `:80` needs to stay free for direct
port-publishing (`run -p 80:80`, `-P`) and can abort daemon start if a web server
already holds it, while `:8080` is the port your *app* most often wants — squat
on it and the proxy fights the very workload it's fronting. `:7879` avoids both.
TLS SNI passthrough is **off by default** (it needs a TLS-serving guest — see
below). Override at install with `PROXY_LISTEN=`/`PROXY_TLS_LISTEN=`/
`PROXY_DOMAIN=`, or `--no-proxy` to skip it. Any free TCP port works; `host:port`
pins an interface (`127.0.0.1:7879` = loopback only).

For a **production ingress** on the standard ports, set `PROXY_LISTEN=:80
PROXY_TLS_LISTEN=:443` at install (or `--proxy-listen :80 --proxy-tls-listen
:443` on the daemon) — it runs as root under systemd, so it binds them without
extra caps, and apps are then reachable at plain `http://web.apps.local/`.

## How it routes

- **`--proxy-domain apps.local`** — an app named `web` is reached at
  `web.apps.local`. The proxy strips the domain suffix to get the app name, then
  resolves that app's current instance. (Empty `--proxy-domain` means the request
  `Host` *is* the app name.)
- **`--port`** — the guest port the proxy forwards to. Defaults from a single
  published/`EXPOSE`d port; set it explicitly when the image exposes several.
- **Resolution is live** (with a ~1s cache), so it never routes to a stale IP: an
  instance's guest IP changes on every re-create/fork, and the app object is the
  source of truth for which instance is current. This is also what makes
  [zero-downtime `app update`](apps.md#zero-downtime-update) work with no proxy
  changes: the reconciler boots the new instance, then flips which instance is
  current once it's ready, and the proxy follows within one resolution TTL while
  the old instance drains — so the cutover drops nothing.

## Two listeners

| Listener | Layer | What it does |
|---|---|---|
| `--proxy-listen` (default `:7879`) | L7 (HTTP) | Routes by the `Host` header, reverse-proxies to the instance (keep-alive, chunked, `X-Forwarded-*`). |
| `--proxy-tls-listen` (off by default) | L4 (TLS SNI) | Reads the TLS ClientHello's SNI and **passes the raw stream through** to the instance — **the guest terminates its own TLS**. The proxy holds no certificates. |

The TLS listener is opt-in because it only works with a guest that serves its own
TLS — enable it with `--proxy-tls-listen :7880` (or `:443`). TLS *termination* at
the proxy (with ACME / custom domains) is later work; today the guest owns its
cert and the proxy just routes by SNI.

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
installer defaults the proxy to `:7879`, clear of the ports you publish to. Put
the proxy on `:80` and publishing *to* host `:80` is no longer available (the
daemon can't bind it twice); that's expected — on a proxy-fronted host you reach
apps by name instead of publishing them to `:80`.

## Model

The proxy runs **in-process** in the daemon (mirroring the DNS proxy), needs
durable apps (`--app-db`), and binds whatever ports you give it — the default
`:7879` needs no privileges, and `:80`/`:443` are bindable because the daemon
runs as root under the systemd unit. See [architecture.md](architecture.md) for
where it sits and [apps.md](apps.md) for the app model it routes to.
