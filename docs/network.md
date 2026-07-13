---
title: Networking
description: "Sandboxes get no network unless you say so. Enable egress with a hostname allowlist, widen it with public CIDRs or full egress, and rely on one invariant: public unicast only."
---

# Networking

A sandbox with no network config gets no NIC and zero egress. That is the out-of-the-box experience; everything below is how you open exactly as much as a workload needs. How the enforcement works under the hood lives in [the network model](concepts/network-model.md).

![crucible default-deny egress](../demo/network.gif)

*A sandbox created with `--net-allow pypi.org` resolves and reaches pypi.org over HTTPS, while every other host is refused at the DNS proxy. The allowlist is the whole reachable surface.*

## Allow specific hostnames

The primary mode: enumerate the hosts the workload may reach. From the CLI:

```bash
crucible run --net-allow pypi.org --net-allow '*.pythonhosted.org' -- pip download requests
```

Over the API, the same policy is the `network` field of the create request:

```json
POST /sandboxes
{
  "vcpus": 1,
  "memory_mib": 512,
  "network": {
    "enabled": true,
    "allowlist": ["pypi.org", "*.npmjs.org", "github.com", "objects.githubusercontent.com"]
  }
}
```

- `network` absent means no network (equivalent to `{"enabled": false}`).
- `network.enabled = false` means no NIC attached; other network fields are ignored.
- `network.enabled = true` with an absent or empty `allowlist` is a `400`. An explicit allowlist is required; unlisted "full internet" is not a supported shape of this mode.

The sandbox response echoes the applied policy along with the assigned addresses:

```json
{
  "id": "sbx_...",
  "network": {
    "enabled": true,
    "allowlist": ["..."],
    "guest_ip": "10.20.0.14",
    "gateway": "10.20.0.13"
  }
}
```

## Allowlist syntax

Two rules, case-insensitive:

- **Exact match:** `pypi.org`.
- **Single-label wildcard:** `*.npmjs.org` matches `registry.npmjs.org` and `www.npmjs.org`, but not `a.b.npmjs.org` or bare `npmjs.org`.

No regex, no ports. Entries are lowercased and trailing dots stripped (`pypi.org.` becomes `pypi.org`). A bare `*` is rejected at config time (that is "all internet" and must be requested explicitly via full egress), and wildcards are only valid in the first label (`*.foo.*.com` is rejected).

## Widen egress for trusted workloads

For an app you deploy yourself, "enumerate every hostname" is often the wrong default. Two opt-ins widen egress without weakening the SSRF guard:

- **Full egress** (`full_egress` in the API, `--net-full-egress` on the CLI): reach any public host. The DNS proxy answers any name and the firewall accepts all destinations except the always-blocked ranges below.
- **CIDR allowlist** (`allowlist_cidr` in the API, `--net-allow-cidr 203.0.113.0/24` on the CLI): reach IP literals in a public prefix directly, which the hostname allowlist cannot express.

> [!CAUTION]
> The invariant for every mode is public unicast only, no exceptions. Metadata and link-local (`169.254.0.0/16`, including `169.254.169.254`), RFC1918, loopback, CGNAT (`100.64.0.0/10`), and the reserved blocks are always dropped. A CIDR overlapping private space has those addresses dropped; a wholly-private CIDR reaches nothing.

## What a denied request looks like

- **A blocked connection** times out silently. There is no ICMP unreachable, so probing for reachable hosts yields no signal.
- **A denied DNS lookup** returns `NXDOMAIN` rather than `REFUSED`, which is less clueful that a filter exists.
- **IP literals never work in allowlist mode.** `curl http://93.184.216.34` does no DNS lookup, so that address was never attested by the DNS proxy and the packet is dropped. Reaching literals is exactly what the CIDR mode is for.

## Inbound isolation

Networking is egress-shaped, but port publish (`-p`) and the [ingress proxy](proxy.md) send traffic toward a guest. Both preserve isolation:

- **Inbound reaches a guest only from the daemon.** The publish forwarder and the proxy dial the guest's IP from the daemon's own network namespace over the sandbox's veth. There is no DNAT and no listener inside the guest's netns that a third party can reach.
- **Peers cannot reach each other.** Each sandbox has its own `/30` in its own netns, and RFC1918 egress is always blocked, so one guest cannot reach a neighbor's guest IP, published port or not.
- **The proxy is not a lateral path.** A guest cannot reach the proxy or publish listeners at all — the host input chain drops every guest-initiated packet to any host-local address (`iifname vh-* drop`, after the established/DNS accepts), even under full egress and even to the host's public IP. So one app can never reach another app's instance through the proxy, published or not; the only route to a guest is the daemon dialing in over its own veth.

## Packet capture (debug)

`crucible sandbox capture <id>` (or `app capture <name>`) streams a live packet
capture of a guest's traffic as standard **pcap** — captured **host-side** on the
sandbox's veth in the root netns, so it needs **no in-guest tcpdump** and works
for distroless/scratch images.

```bash
crucible app capture web --filter 'tcp port 8080' -w web.pcap   # then open in Wireshark
crucible sandbox capture sbx_… --max-seconds 30 | wireshark -k -i -
```

- **Bounded always** — `--max-bytes` (default 50 MiB) and `--max-seconds`
  (default 60 s) cap every capture; `--snaplen` sets per-packet bytes.
- **Filter** is a BPF expression, validated (no shell/option injection) and passed
  to the host's `tcpdump`.
- **Gated + audited** — payloads are sensitive, so it requires a token granted the
  default-deny [`capture`](policy.md) scoped op (never implied by `read`), and the
  daemon logs every capture (token, instance, filter, caps). Needs `tcpdump` on the
  host.

> [!NOTE]
> Design goals, the packet flow, per-sandbox setup order, failure modes, and the deliberate non-goals (IPv6, port filters, rate limiting) live in [the network model](concepts/network-model.md).
