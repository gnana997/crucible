---
title: TLS & custom domains
description: "Automatic HTTPS for apps: the ingress proxy terminates TLS with a managed certificate, issues and renews it over ACME, and routes both generated and custom domains."
---

# TLS termination & custom domains

The [ingress proxy](proxy.md) routes many apps by name. This page covers the
`:443` half of it: how the proxy **terminates TLS** with a certificate it
manages for you — issued and renewed automatically over ACME (Let's Encrypt) —
so an app is reachable over HTTPS with no certificate work in the guest, on both
the generated `<app>.<proxy-domain>` name and any **custom domain** you attach.

Two ways an app can get HTTPS on `:443`:

| Mode | Who holds the cert | When to use |
|---|---|---|
| **terminate** (default) | The **proxy** — a managed cert, auto-issued and renewed. The guest sees plain HTTP. | A normal HTTP app. You want HTTPS without touching the guest. |
| **passthrough** | The **guest** — the proxy pipes the raw TLS stream through by SNI and never sees the plaintext. | The guest already serves its own TLS, or speaks a non-HTTP TLS protocol. |

Termination is the default; passthrough is [described in the proxy
doc](proxy.md#two-listeners) and selected per app with `--tls-mode passthrough`.

## Enable it

Termination needs the TLS listener open **and** a signal that you want the proxy
to manage certs. Open the listener with `--proxy-tls-listen`, then set **either**
`--acme-email` (automatic certs) **or** `--cert-dir` (manual certs / storage).
With the listener open but neither set, `:443` stays passthrough-only — exactly
the pre-v0.7.0 behavior.

```bash
crucible daemon … \
  --proxy-listen :80 --proxy-tls-listen :443 \
  --proxy-domain apps.example.com \
  --acme-email ops@example.com
```

```bash
crucible app create web --image nginx:alpine --port 80
# reachable over HTTPS at https://web.apps.example.com/ — the cert is
# issued on the first handshake and renewed in the background.
```

The daemon runs as root under the systemd unit, so it binds `:80`/`:443` without
extra capabilities. `:80` is required whenever termination is on — it serves the
ACME HTTP-01 challenge and 301-redirects plain HTTP to HTTPS (see below). At
install time, set `PROXY_LISTEN=:80 PROXY_TLS_LISTEN=:443` and pass
`--acme-email` in the daemon config to turn this on for a production ingress.

### Daemon flags

| Flag | Meaning |
|---|---|
| `--proxy-tls-listen :443` | Opens the TLS listener. Required for either termination or passthrough. |
| `--acme-email <addr>` | ACME account email. Setting it enables **automatic HTTPS** (Let's Encrypt) and termination. Empty = no ACME. |
| `--acme-ca production\|staging` | Which Let's Encrypt endpoint (default `production`). Use `staging` while testing — it has far higher rate limits but issues untrusted certs. |
| `--acme-ca-url <url>` | Override the ACME directory URL outright (e.g. a private CA). Takes precedence over `--acme-ca`. |
| `--acme-ca-root <file>` | PEM bundle of root CA(s) to trust **for the ACME server** — only for a private/test CA whose endpoint isn't publicly trusted. Doesn't affect image pulls or other daemon TLS. |
| `--cert-dir <path>` | Storage for certs, keys, and ACME account state (default `/var/lib/crucible/certs` when termination is enabled). Setting it alone (no `--acme-email`) enables manual-cert mode. |

## Domains

An app is always reachable at its **generated** name under the proxy domain —
`web` → `web.apps.example.com` — and the managed cert covers it automatically.

To serve an app on a **domain you own**, point the domain's DNS at the daemon
host and attach it:

```bash
crucible app domain add web shop.example.com
crucible app domain ls web
crucible app domain rm web shop.example.com
```

A domain is **globally unique** across apps — attaching one that's already taken
fails. Once attached (and once DNS resolves to the host), the proxy issues a cert
for it on the first HTTPS request and routes it to the app, the same as the
generated name.

### Issuance is gated to your domains

On-demand issuance only ever fires for a name that maps to a **real,
terminate-mode app** — the generated `<app>.<proxy-domain>` of a live app, or a
custom domain you've attached. A stray or hostile SNI hitting `:443` gets **no
certificate** (the handshake is refused). This is deliberate: unbounded issuance
would be an abuse vector and would burn the CA's rate limits. There is nothing to
configure — it's enforced on every handshake.

## How a request flows

- **`:443`** peeks the TLS ClientHello's SNI. For a terminate-mode app it
  **terminates** with the managed cert and reverse-proxies plain HTTP to the
  guest (keep-alive, `X-Forwarded-*`, as on the [HTTP listener](proxy.md#how-it-routes)).
  For a passthrough app it pipes the raw stream to the guest untouched.
- **`:80`** serves ACME **HTTP-01** challenges, and for a terminate-mode app
  **301-redirects** every other request to `https://…`. Opt an app out with
  `--no-https-redirect` (it then serves plain HTTP on `:80`) — only meaningful
  under termination; a passthrough app owns `:80`/`:443` itself.
- **Challenges** use **TLS-ALPN-01** (negotiated on `:443`) and **HTTP-01** (on
  `:80`); whichever the CA offers is answered automatically.
- **Renewal** runs in the background well before expiry — no cron, no reload.

## Certificate status

Every domain's certificate state is observable, so a domain whose DNS isn't
pointed at the host (issuance failing) shows up rather than silently not working:

```bash
crucible app domain ls web
# DOMAIN                KIND       TLS        CERT      EXPIRES
# web.apps.example.com  generated  terminate  active    2026-10-13
# shop.example.com      custom     terminate  active    2026-10-13
# new.example.com       custom     terminate  pending   -
# bad.example.com       custom     terminate  failed: … -
```

The states:

| State | Meaning |
|---|---|
| `active` | a valid managed cert is being served (with its expiry) |
| `expiring` | active, but inside the renewal lead — renewal is due/underway |
| `pending` | terminate-mode, no cert yet (issuance in flight, or the domain hasn't been requested) |
| `failed` | the last ACME attempt errored — the error is shown (commonly the domain's DNS isn't pointed at the host) |
| `manual` | served from a drop-in manual cert (never auto-renewed) |
| `passthrough` | the app is passthrough-mode; the guest owns its cert |

The same data is on the API (`GET /apps/{name}/domains?detail=1` → a `details`
array) and as [Prometheus/OTLP metrics](observability.md#tls-certificate-status)
(`app_cert_state`, `app_cert_not_after_seconds`) for alerting on expiry or a
failed renewal. The default `GET /apps/{name}/domains` (no `?detail`) still
returns the plain name list, unchanged.

## Manual certificates

To serve your own certificate for a domain instead of ACME, drop a matching
`<name>.crt` + `<name>.key` PEM pair into `<cert-dir>/manual/`:

```
/var/lib/crucible/certs/manual/shop.crt
/var/lib/crucible/certs/manual/shop.key
```

They're loaded at daemon start and served by SNI, but never renewed — rotating
them is your job. Manual and ACME certs coexist: a manual cert wins for the
domains it covers; everything else falls through to on-demand ACME. A `.crt`
without a matching `.key` is ignored.

## Per-app options

Set these at `app create` (they live on the app spec and survive redeploys):

| Flag | Default | Effect |
|---|---|---|
| `--tls-mode terminate` | terminate | The proxy manages the cert; the guest sees plain HTTP. |
| `--tls-mode passthrough` | — | The guest owns its TLS; the proxy routes by SNI and never decrypts. No cert is issued for the app. |
| `--no-https-redirect` | redirect on | Serve plain HTTP on `:80` instead of 301-redirecting to HTTPS. Only meaningful with termination. |

## Notes

- **Wildcards / DNS-01** aren't issued yet — each generated and attached name
  gets its own cert via HTTP-01 / TLS-ALPN-01. Point real DNS at the host first;
  a name that doesn't resolve to the daemon can't complete a challenge.
- **Staging first.** When trying this against a real domain, start with
  `--acme-ca staging` to avoid the production rate limits, then switch to
  `production` once the flow works end-to-end.
- **Isolation is unchanged.** Terminating TLS at the proxy doesn't loosen the
  per-sandbox network boundary in [network.md](network.md): the proxy still dials
  the guest from the host namespace, and guests still can't reach the proxy or
  each other.
