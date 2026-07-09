# Contributing to crucible

Thanks for your interest in crucible. It's an early (pre-1.0) project, so the most valuable contributions right now are bug reports with reproductions, correctness fixes, and docs — but PRs of any size are welcome. This guide covers how to build, test, and match the house style. By participating you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Ground rules

- **Discuss non-trivial changes first.** For anything beyond a bug fix or docs, open an issue to sketch the approach before writing a lot of code — it saves everyone a wasted review cycle.
- **Keep the isolation model honest.** crucible runs untrusted code; changes to the sandbox, jailer, networking, or clone-safety paths get extra scrutiny. Don't weaken a boundary for convenience.
- **Security issues go privately**, not through public issues or PRs — see [SECURITY.md](SECURITY.md).

## Prerequisites

**To build:** Go **1.25+**. That's it — the daemon and agent are pure Go.

**To actually run a sandbox** you need a Linux host with:

- KVM available (`/dev/kvm`),
- a [Firecracker](https://github.com/firecracker-microvm/firecracker) binary,
- an uncompressed guest kernel (`vmlinux`),
- an ext4 rootfs image with `crucible-agent` baked in (see `make rootfs`),
- and, for snapshot/fork and per-sandbox networking, **root** (the jailer and netns/nftables setup require it).

macOS/Windows can build and run the unit tests, but booting a VM requires Linux + KVM.

## Building

```bash
make build     # build the crucible daemon → ./crucible
make agent     # build the guest agent (static linux/amd64) → ./bin/crucible-agent
make rootfs BASE_ROOTFS=assets/ubuntu.squashfs   # bake the agent into an ext4 rootfs
```

Run `make help` for the full target list.

## Running the daemon

```bash
sudo ./crucible daemon \
  --firecracker-bin /usr/local/bin/firecracker \
  --kernel  ./assets/vmlinux \
  --rootfs  ./assets/rootfs.ext4 \
  --jailer-bin /usr/local/bin/jailer          # enables snapshot/fork
```

Useful flags: `--listen` (default `127.0.0.1:7878`), `--log-format text|json`, `--log-level`, `--network-egress-iface eth0` (enables per-sandbox networking), `--network-subnet-pool`, `--dns-upstream`. `--no-wait-for-agent` is a dev-only escape hatch for rootfs images without the agent. See `crucible daemon --help` for the complete set.

## Testing

```bash
make test      # go test ./...
make race      # go test -race ./...   (what CI runs)
make vet       # go vet ./...
```

Most packages are unit-tested with fakes and don't need a VM. Tests that require KVM/root are the exception; if you touch the runner, jailer, or network packages, exercise the affected flow end-to-end rather than only typechecking. The `scripts/` directory has smoke tests for the paths that are hard to unit-test:

```bash
scripts/smoke_e2e.sh            # create → exec → delete
scripts/smoke_fork.sh           # snapshot → fork
scripts/smoke_clone_safety.sh   # per-fork RNG/identity divergence
scripts/smoke_restart.sh        # registry reconcile across a daemon restart
```

## Style and conventions

CI enforces three things, so run them before pushing:

```bash
make fmt       # gofmt -s -w .   (CI fails on any gofmt -s diff)
make vet
make lint      # golangci-lint (config in .golangci.yml)
```

The lint set is `errcheck, govet, ineffassign, staticcheck, unused, revive, gocritic, misspell`, plus `gofmt`/`goimports` formatting. Beyond the linters:

- **Match the surrounding code.** Follow the existing idiom — the `success bool` + deferred-cleanup rollback pattern for multi-step operations, the load→vsock→rootfs→resume ordering in the runner, careful handle concurrency. New code should read like the code next to it.
- **Don't over-build.** No abstractions, helpers, or error handling beyond what the change needs. Validate at system boundaries (HTTP input, syscalls, subprocess results); trust internal callers.
- **Comment the *why*, not the *what*.** The existing comments explain non-obvious decisions (why a write deadline is cleared, why an artifact is staged private-per-fork). Keep that bar.
- **Tests belong with the change.** Add or update tests in the same PR; if a behavior is hard to unit-test, say how you exercised it.

## Submitting a PR

1. Branch off `main`.
2. Keep the PR focused — one logical change. Split unrelated cleanup into its own PR.
3. Make sure `make fmt vet test` (and `make lint` if you have golangci-lint) is clean; CI runs `go vet`, a `gofmt -s` check, and `go test -race` on Go 1.25.
4. Write a clear description: what changed, why, and how you verified it. Link the issue if there is one.

## Cutting a release

Maintainers: see [RELEASING.md](RELEASING.md). The trap worth knowing even as a
contributor — the guest agent is **baked into profile rootfs images at build
time** (`make rootfs` / `make profile`) but **injected into OCI images by the
daemon** (`make build`). So after changing `cmd/crucible-agent`, a `make build`
alone leaves every profile `.ext4` on the old agent.

## License

By contributing you agree that your contributions are licensed under the project's [Apache-2.0 license](LICENSE).
