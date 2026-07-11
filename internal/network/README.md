# internal/network

Developer notes for the networking subsystem. The user-facing behavior is
documented in [docs/network.md](../../docs/network.md); the design in
[docs/concepts/network-model.md](../../docs/concepts/network-model.md).

## Package layout

```
internal/network/
  doc.go              package doc
  manager.go          owns the subnet pool + DNS proxy; wires to sandbox.Manager; orphan reap
  subnet.go           /30 pool
  bitmap.go           allocation bitmap
  netns.go            netns create/delete (shells out to `ip netns`)
  veth.go             veth pair + bridge + tap setup inside the netns
  nft.go              nftables rule emission via `nft -f -`
  allowlist.go        trie + pattern matching
  exec.go             namespaced command execution helpers
  reap.go             startup orphan reap (netns + nft)
  dhcp/               per-netns DHCP responder (responder.go, wire.go)
  dnsproxy/           UDP DNS proxy + upstream forwarding (proxy.go, upstream.go)
```

## Testing

- **Unit** (`allowlist.go` tests): trie construction, wildcard matching, reject-bad-input, normalization.
- **Unit** (`subnet.go` / `bitmap.go` tests): the allocator hands out unique `/30`s, releases them on delete, and rejects exhaustion.
- **Unit** (`dnsproxy` tests): an in-process upstream stub exercises allowed / denied / NXDOMAIN-on-no-match / set-update-on-allowed cases.
- **End-to-end** (`scripts/smoke_e2e.sh`): default-deny, allowlisted (allowed / denied / IP-literal / `*.domain`), and per-fork networking against a live daemon.

## Dependencies

- **`golang.org/x/sys/unix`**: netns entry (`Setns`) and syscall work.
- **`github.com/miekg/dns`**: DNS wire format + the upstream client. Chosen over rolling our own after weighing EDNS0, TCP fallback, and CNAME-chain walking against the RFC-1035 machinery it would require; used by CoreDNS and ExternalDNS, pulls only `golang.org/x/net`.
- **DHCP is hand-rolled**: the protocol is narrow (one MAC, one lease, two request types) and stable; the responder is ~260 LOC with no library pulled in.
- **No netlink library**: namespace/link/nftables manipulation shells out to `ip` and `nft`. Readable and debuggable; subprocess latency is a non-issue at per-sandbox-create-once granularity.
