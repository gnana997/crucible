# Network: default-deny with per-sandbox allowlist

> Design doc for the v0.1 network feature. The doc is deliberately concrete — it describes the implementation we intend to build, not every possible alternative. Supersedes the brief mention in [VISION.md](VISION.md). For the higher-level "why network isolation?" see the FAQ in [CRUCIBLE_README.md](../../CRUCIBLE_README.md).

## Goals (v0.1)

1. **Default-deny.** A sandbox with no network config gets no NIC attached and zero egress reachability. This is the out-of-the-box experience.
2. **Hostname allowlist override.** A sandbox configured with `network.enabled=true` and an allowlist of hostname patterns can reach exactly those hostnames (A/AAAA records only) over any TCP/UDP port. Everything else — ICMP to arbitrary hosts, egress to IP literals, connections to ports on resolved IPs that we didn't answer for — is dropped.
3. **Enforcement on the host kernel.** Policy is applied in the host's nftables and a host-side DNS proxy that the guest is forced to use. The guest itself is untrusted — if user code escalates to root and tears down guest-side firewall rules, the host rules still block egress.
4. **Per-sandbox isolation.** Sandbox A cannot see, reach, or influence sandbox B's network traffic, even if both are allowlisted to overlapping destinations.
5. **Clean lifecycle.** Create → use → delete leaves no orphan namespaces, veth pairs, nftables tables, or DNS proxy state. Daemon-crash recovery wipes stale per-sandbox network state on startup.

## Non-goals (v0.1)

- IPv6 (deferred; add-on later). All allocation and rules are IPv4-only.
- CIDR-based allowlists (`10.0.0.0/8`). Hostname-only for v0.1.
- Port allowlists (`pypi.org:443` only). We allow any port to allowed IPs; we don't try to constrain ports.
- Protocol allowlists. TCP, UDP, ICMP all allowed to allowed IPs — no per-protocol filter.
- Rate limiting. No egress rate limit per sandbox.
- Packet capture / traffic logging. The v0.2 "Packet capture on demand" item is exactly this.
- Bring-your-own-DNS (forcing a specific upstream resolver per sandbox). All sandboxes share the same upstream.
- Policy files. Configuration is per-request JSON; YAML policy files are a v0.2 item.
- Running the DNS proxy in a separate process. It's in-proc for v0.1.

## Architecture

```
                          ┌─────────────────────────────────────────────────────┐
                          │                  Host root netns                    │
                          │                                                     │
                          │   crucible-dns dummy iface: 10.20.255.254/32        │
                          │                        │                            │
                          │   ┌────────────────────▼───────────────────────┐    │
┌────────────┐  veth-A-h  │   │  DNS proxy (miekg/dns)                     │    │
│ sandbox A  │◄───────────┼───┤  single UDP listener on 10.20.255.254:53   │    │
│ netns      │ 10.20.0.1  │   │  policies sync.Map[guest-src-IP → policy]  │    │
│ guest:     │            │   │  upstream: --dns-upstream (system | IP)    │    │
│  10.20.0.3 │            │   └───────────────┬────────────────────────────┘    │
└────────────┘            │                   │                                 │
                          │                   └──► upstream resolver :53        │
┌────────────┐  veth-B-h  │                                                     │
│ sandbox B  │◄───────────┤   ┌─────────────────────────────────────────────┐   │
│ netns      │ 10.20.1.1  │   │  nftables inet table "crucible":            │   │
│ guest:     │            │   │    per-sandbox chain keyed on iifname       │   │
│  10.20.1.3 │            │   │    - accept udp/53 to 10.20.255.254         │   │
└────────────┘            │   │    - accept to @sandbox-<id>-allowed set    │   │
                          │   │    - drop rest                              │   │
                          │   │  postrouting: masquerade on egress iface    │   │
                          │   └─────────────────────────────────────────────┘   │
                          └─────────────────────────────────────────────────────┘
```

**The DNS anycast IP.** We allocate `10.20.255.254` inside the subnet pool as a reserved host-side address, bound to a dummy interface in the host root netns. Every sandbox gets a route `10.20.255.254/32 via <its own gateway>` so DNS queries traverse the veth into host root netns and land on the single shared listener. Source IP of the incoming packet — the guest's `.3` — identifies the sandbox, unambiguously, because every sandbox owns a unique /30. O(1) sync.Map lookup maps source IP to the sandbox's policy.

Each sandbox gets:

- **Its own network namespace** on the host.
- **A veth pair**: one end (`veth-<id>-h`) stays in the host root netns, the other end (`veth-<id>-g`) is moved into the sandbox netns.
- **A tap device** inside the sandbox netns, which Firecracker uses as the guest's NIC.
- **A /30 subnet** from the 10.20.0.0/16 pool: .0 network, .1 host-side veth (also the guest's default gateway and DNS target), .2 guest-side veth... wait — we flip this. Actually let's use /30 cleanly: `.1` is host-side, `.2` is guest-side veth-to-tap routing, `.3` is the guest IP. The guest talks to `.1` for DNS and egress.

(The actual addressing is boring; the point is each sandbox has a dedicated small subnet and the host end is a stable gateway address.)

The daemon holds three shared host-root-netns resources, allocated once at startup:

- **The `crucible-dns` dummy interface**, carrying the reserved anycast IP `10.20.255.254/32` that every sandbox routes DNS to.
- **The DNS proxy**, one UDP listener bound to `10.20.255.254:53`, using `miekg/dns` for wire format. Policies keyed by guest source IP in a `sync.Map` — O(1) read path, no mutex on the hot path.
- **A single nftables `inet` table** named `crucible`, containing a per-sandbox set of allowed IPs and a chain per sandbox for the filter rules.

## Per-sandbox setup (on Manager.Create)

Order matters — each step assumes the previous has succeeded; failures trigger rollback that unwinds in reverse.

1. **Allocate a /30 from the pool.** Bitmap in the Manager. Rejects create if exhausted (cap around 16K concurrent sandboxes, plenty).
2. **Create network namespace.** `ip netns add crucible-<sandbox-id-sanitized>` (shell out for v0.1; switch to netlink if we outgrow it).
3. **Create veth pair.** `ip link add veth-<id>-h type veth peer name veth-<id>-g`. Move `veth-<id>-g` into the sandbox netns.
4. **Assign IPs and bring links up.**
   - Host side: `ip addr add 10.20.X.1/30 dev veth-<id>-h; ip link set veth-<id>-h up`.
   - Guest side (in netns): `ip addr add 10.20.X.2/30 dev veth-<id>-g; ip link set veth-<id>-g up; ip link set lo up`.
5. **Create tap inside the sandbox netns.** `ip tuntap add tap-<id> mode tap; ip link set tap-<id> up`. Firecracker will attach here.
6. **Route inside the sandbox netns.**
   - Default route: `10.20.X.1` via `veth-<id>-g`. But wait — Firecracker will be on the tap. The guest kernel sees the tap as its NIC. We need:
     - Tap bridged to veth-<id>-g? Simpler: a bridge inside the netns joining tap and veth-g. Then guest IP `.3` lives on the same L2 as the host-side veth `.1` routing.
     - Or: just a Layer-3 relay. Put tap and veth-g in the same bridge; assign bridge the `.2` address. Guest IP is `.3`, gateway `.1`.
   - Simpler + cleaner: **bridge inside the netns**. `ip link add br-<id> type bridge; ip link set veth-<id>-g master br-<id>; ip link set tap-<id> master br-<id>; ip addr add 10.20.X.2/30 dev br-<id>` and the guest gets `.3` with gateway `.1`.
7. **Register in the nftables `crucible` table.**
   - Add an IP set `sandbox-<id>-allowed` (type `ipv4_addr`, `flags timeout`).
   - Add a chain `sandbox-<id>` with rules:
     - `iifname "veth-<id>-h" ip daddr 10.20.X.1 udp dport 53 accept` — DNS to the proxy.
     - `iifname "veth-<id>-h" ip daddr @sandbox-<id>-allowed accept` — allowed IPs.
     - `iifname "veth-<id>-h" drop` — everything else from this sandbox.
   - Hook into the `forward` chain via a rule that jumps to `sandbox-<id>` when source matches the sandbox's subnet.
   - Masquerade (one shared rule in `postrouting` on the egress interface) handles NAT for all sandboxes.
8. **Register with the DNS proxy.** `proxy.Register(sandboxID, sourceIP=10.20.X.3, allowlist=[...])`. The proxy now knows: queries arriving from 10.20.X.3 should be filtered against that allowlist.
9. **Tell jailer to use this netns.** Pass `--netns /var/run/netns/crucible-<sandbox-id-sanitized>` to the jailer argv. Firecracker joins the netns on exec.
10. **Configure Firecracker's network interface.** `PUT /network-interfaces/eth0` with `host_dev_name=tap-<id>` and a guest MAC we generate. The guest IP `.3` + gateway `.1` + DNS `.1` we either burn into the guest image at rootfs-build time, or we set them via a small init that reads them from vsock metadata on boot. **Lean: bake a `crucible-network` systemd unit in the rootfs that reads config from a virtio-mmio device or a vsock metadata channel** — but that's a later weekend. For v0.1, **use kernel boot-args to DHCP**, and run a tiny DHCP server in each sandbox netns that answers exactly one lease. This is uglier but requires zero changes to the rootfs.

### Guest IP configuration: per-netns DHCP + agent-driven refresh on fork

**Initial boot:** guest runs `dhclient` (baked into the rootfs alongside `crucible-agent`). A hand-rolled DHCP responder per netns answers DISCOVER/REQUEST for the guest's known MAC with the sandbox's pre-assigned IP + gateway + DNS (10.20.255.254) + a short lease (60s). Responder enters the target netns via `runtime.LockOSThread` + `unix.Setns(CLONE_NEWNET)` before binding UDP/67.

**Fork resume:** snapshot captures the source's eth0 config (source's IP, source's gateway). After the fork's Firecracker completes `LoadSnapshot` and the VM resumes, the guest thinks it still has the source's IP in the source's subnet — neither of which is reachable from the fork's new netns. Without intervention, the guest is "dark" until dhclient's next scheduled renewal (up to 30s).

We fix this by having **`crucible-agent` expose a `POST /network/refresh` endpoint over vsock**. The host's `sandbox.Manager.Fork` invokes this immediately after resume; the agent runs `dhclient -r eth0` (release) followed by `dhclient -1 eth0` (one-shot acquire), which DISCOVER/OFFER/REQUEST/ACKs against the fork's per-netns DHCP responder and reconfigures eth0 with the fork's assigned IP + gateway + DNS. Adds roughly one DHCP round-trip (~100–300 ms) to fork cost — invisible next to the existing snapshot-restore overhead.

Failure mode: if the agent isn't available (old rootfs without dhclient) or the RPC fails, `Manager.Fork` logs a warning and moves on. Fork's guest will recover via the normal 60s lease-renewal timer.

## Packet flow

**Scenario A: allowed DNS query.** Guest does `getaddrinfo("pypi.org")`. Glibc reads `/etc/resolv.conf`, sees nameserver `10.20.X.1`, sends a UDP DNS query from `10.20.X.3:random` to `10.20.X.1:53`. The packet traverses tap → bridge → veth-g → host root netns. nftables: matches `dport 53 accept`, forwards to the DNS proxy. Proxy looks up the source IP (10.20.X.3) → sandbox ID → allowlist. `pypi.org` matches `pypi.org`; proxy forwards the query to upstream (1.1.1.1), gets the A record (e.g. 151.101.0.223), adds that IP to `sandbox-<id>-allowed` set with a TTL equal to the DNS answer's TTL (clamped to a sensible floor), returns the response to the guest.

**Scenario B: allowed HTTP request.** Guest initiates TCP to 151.101.0.223:443. Packet leaves tap, traverses to host netns forward chain. nftables: matches `ip daddr @sandbox-<id>-allowed accept`. Packet forwards to egress interface; masquerade rewrites source IP to host's primary interface IP; packet leaves the host. Return packets hit conntrack (stateful) and are un-masqueraded back to the guest.

**Scenario C: denied destination.** Guest tries TCP to 1.2.3.4:22 (not in allowlist; we never resolved anything to that IP). Packet leaves tap, hits nft `sandbox-<id>` chain. No match on `dport 53`. Not in `@sandbox-<id>-allowed`. Falls through to `drop`. Guest sees a connection timeout. No ICMP unreachable (we drop silently — attackers probing for what's reachable get no signal).

**Scenario D: denied DNS query.** Guest resolves `evil.example.com`. DNS proxy sees query from known source IP, looks up allowlist, no match. Proxy returns `NXDOMAIN` (not `REFUSED` — less clueful that a filter exists). Guest gets NXDOMAIN, fails lookup, never even tries to connect.

**Scenario E: IP literal.** Guest does `curl http://93.184.216.34` (example.com's IP). No DNS lookup happens. Packet goes to forward chain. 93.184.216.34 isn't in the allowed set (no DNS answer ever added it). Dropped. This is the point of the design — the allowlist pivots on *DNS-attested* IPs, so IP literals never work unless the user resolved a hostname that answered with that IP.

## Allowlist syntax & matching

**v0.1 grammar.**

- Exact match: `pypi.org`. Case-insensitive.
- Single-label wildcard: `*.npmjs.org` matches `registry.npmjs.org` and `www.npmjs.org`. Does **not** match `a.b.npmjs.org` or bare `npmjs.org`.

That's it. Two rules. No regex, no CIDR, no port numbers.

**Matching algorithm.** Build a trie keyed by reversed DNS labels per sandbox's allowlist. Query `registry.npmjs.org` → look up `org.npmjs.registry` in the trie. Match if any prefix of the query ends in an exact entry OR in a single-label wildcard entry at the matching depth. O(labels) per query.

**Corner cases.**

- `*` on its own is rejected at config time (too broad — that's "all internet" and should be `enabled: true, allowlist: [...everything...]` explicitly).
- Allowlist entries with uppercase letters are normalized to lowercase.
- Trailing dots stripped (`pypi.org.` → `pypi.org`).
- Wildcards only allowed as the first label. `*.foo.*.com` is rejected.

## API shape

Frozen wire contract (shipped already as `ImageRef`-style stub in createSandboxRequest, needs populating now):

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

**Field semantics.**

- `network` absent → no network, equivalent to `{"enabled": false}`.
- `network.enabled = false` (or absent) → no NIC attached, all other network fields ignored.
- `network.enabled = true`, `allowlist` absent or empty → **rejected with 400.** We require an explicit allowlist. "Full internet" is not a supported v0.1 config — users who need it can open a FR. (Rationale: default-deny ethos; make "full internet" require an explicit gesture we don't implement yet.)
- `network.enabled = true`, `allowlist` populated → apply per this doc.

**Response.** The `sandboxResponse` gains a `network` substruct describing the applied policy (helpful for debugging):

```json
{
  "id": "sbx_...",
  "network": {
    "enabled": true,
    "allowlist": ["..."],
    "guest_ip": "10.20.0.3",
    "gateway": "10.20.0.1"
  }
}
```

## Lifecycle integration

Where the new work plugs into existing flows:

- **Manager.Create** — after `jailer.Stage` completes, before running jailer: allocate subnet, set up netns, veth, tap, nftables, register with DNS proxy. Pass the netns path to JailerRunner.
- **JailerRunner.Start** — accept `NetNS` field on `runner.Spec`. Pass as `--netns` to jailer argv.
- **Manager.Delete** — after `handle.Shutdown()` (which handles chroot + cgroup cleanup): tear down nftables chain/set, deregister from DNS proxy, delete netns (which also kills the veth pair). Best-effort + idempotent.
- **Manager.Snapshot** — network state is *host-side*; snapshots don't capture it. No changes required.
- **Manager.Fork** — each fork gets its own subnet/netns/veth; inherits the source's allowlist policy. The fork's DHCP server hands it a new IP; Firecracker's snapshot state recorded the source's eth0 MAC + IP, so on restore the fork's kernel has stale network config and needs to rediscover. DHCP handles this via the standard "rebind on resume" path. (This is the detail worth smoke-testing — may push us toward option 3 / agent-based reconfig if DHCP doesn't re-issue on snapshot restore.)
- **Daemon startup (orphan reap)** — list all netns named `crucible-*`, all nft chains in the `crucible` table, wipe everything. Similar to the existing jailer orphan reap.

## Failure modes

| Failure | Blast radius | Response |
|---|---|---|
| DNS proxy crashes | All sandboxes lose DNS | Log loudly, kill daemon. Systemd unit restarts it. (Fail-closed beats mystery behavior.) |
| `nft` command fails during create | One sandbox | Roll back: delete netns, release subnet, return 500 to caller. |
| Netns creation fails (EPERM / EBUSY) | One sandbox | Same. |
| Subnet pool exhausted | One sandbox | Reject with 429 + clear error ("no network subnets available; delete some sandboxes"). |
| Guest tries to reach non-allowed IP | Expected | Drop silently. nft counter increments — visible in future `/metrics`. |
| Allowlist syntax invalid | One sandbox | Reject with 400 at create time, never reach setup. |
| DHCP server in netns dies | One sandbox | Guest loses lease → loses network when lease expires. For v0.1 we give huge lease (24h); revisit if an issue. |

## Testing strategy

- **Unit tests** (`internal/network/allowlist_test.go`): trie construction, wildcard matching, reject-bad-input cases, normalization.
- **Unit tests** (`internal/network/subnet_test.go`): allocator allocates unique /30s, releases on delete, rejects exhaustion.
- **Unit tests** (`internal/network/dnsproxy_test.go`): use an in-process upstream stub; exercise allowed / denied / timeout-exceeded / NXDOMAIN-on-no-match / set-update-on-allowed cases.
- **Integration test** (root-only; gated by `-tags=integration`): spin up a real netns, set up veth + nft + DNS proxy, run `curl` inside the netns against a known-allowed + known-denied IP, assert the allowed one succeeds and denied times out.
- **End-to-end** ([smoke_fork.sh](../scripts/smoke_fork.sh)): extend with a "network" variant that creates a sandbox with `allowlist: ["example.com"]`, execs `curl https://example.com` (expect 200) and `curl https://google.com` (expect timeout).

## Package layout

```
internal/network/
  doc.go              package doc
  subnet.go           /30 pool allocator
  subnet_test.go
  netns.go            netns create/delete (shell out to `ip netns`)
  veth.go             veth pair + bridge + tap setup inside netns
  nft.go              nftables rule emission via `nft -f -`
  allowlist.go        trie + pattern matching
  allowlist_test.go
  dnsproxy/
    proxy.go          UDP DNS proxy, per-source-IP policy lookup
    proxy_test.go
    upstream.go       forwarding to 1.1.1.1 (or configurable)
  manager.go          owns subnet pool, DNS proxy, reap-orphans, wires to sandbox.Manager
```

## Dependencies

- **`golang.org/x/sys/unix`** — already in. For any netlink/syscall work.
- **Shell out to `ip` and `nft`** — for v0.1. Both are standard on any distro that can run Firecracker, and shelling is easier to read/debug than the netlink/libmnl dance. Move to pure-Go netlink in a later pass if it bites.
- **One new direct dep: `github.com/miekg/dns`** for DNS wire format + upstream client. Roll-our-own was rejected after weighing EDNS0, TCP-fallback, and CNAME-chain-walking requirements against the ~500–800 LOC of RFC-1035 machinery we'd need to write and harden. miekg/dns is used by CoreDNS + ExternalDNS; transitively pulls only `golang.org/x/net` (and `golang.org/x/sys`, which we already have).
- **DHCP stays hand-rolled.** Protocol is narrow (one MAC, one lease, two request types), frozen in practice (RFC 2131 from 1997), and the responder is a natural teaching artifact. ~260 LOC total; no library pulled in.
- **No netlink library.** All network-namespace/link/nftables manipulation shells out to `ip` and `nft` (v0.1). Debuggable and readable; subprocess latency is a non-issue at per-sandbox-create-once granularity.
