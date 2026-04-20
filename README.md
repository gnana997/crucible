# crucible

> Sandbox runtime for AI coding agents. Firecracker microVMs, a single Go binary, snapshot/fork as first-class primitives.

![Status: v0.1-dev](https://img.shields.io/badge/status-v0.1--dev-orange)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)
![Core: Go](https://img.shields.io/badge/core-Go-00ADD8)

> **Status.** `crucible daemon` boots Firecracker microVMs and manages their lifecycle over HTTP — you can `POST /sandboxes` and get a running VM back. You can't run commands *inside* the VM yet — exec support via vsock lands next. Don't use this for anything real.

## Why this exists

AI coding agents write code and they want to run that code — to check compile, run tests, try three approaches in parallel. The options today are all wrong in different ways: raw Docker (shared kernel, weak isolation, no fork), hosted sandbox services (lock-in, cost scales with usage), or rolling your own Firecracker stack (months of operational work).

crucible is the fourth option: a single self-hosted Go binary on top of Firecracker, with snapshot/fork as first-class primitives, sane quotas and observability baked in, defaults tuned for AI-generated code.

Full motivation, design, and FAQ: [docs/VISION.md](docs/VISION.md).

## What works today

| Capability | Status |
|---|---|
| Go module + CLI skeleton | ✅ done |
| Firecracker runner (boot VM from config) | ✅ done |
| HTTP API — sandbox lifecycle (create / list / get / delete) | ✅ done |
| JSON lifecycle logs (`--log-format=json`) | ✅ done |
| Graceful SIGTERM drain of active sandboxes | ✅ done |
| HTTP API — exec inside sandbox (via vsock) | 🔨 next |
| Resource quotas (beyond VM sizing) | ⏳ planned |
| Snapshot + fork primitives | ⏳ planned |
| Default-deny network + allowlist | ⏳ planned |
| Structured execution record per exec | ⏳ planned |
| Prometheus `/metrics` endpoint | ⏳ planned |
| Python SDK | ⏳ planned |
| Install script + systemd unit | ⏳ planned |

Full trajectory through v1.0: [docs/ROADMAP.md](docs/ROADMAP.md).

## Try it locally

Requirements:

- Linux host with KVM (x86_64). `ls /dev/kvm` succeeds and is readable.
- Go 1.24+ (to build).
- Firecracker v1.15+ binary, a guest kernel (uncompressed `vmlinux`), and a rootfs (`.ext4`). See the [Firecracker getting-started guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md) for obtaining the kernel and rootfs, or pull them from [Firecracker's CI bucket](https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.11/x86_64/).

Build and start the daemon:

```bash
make build
./crucible daemon \
  --firecracker-bin /path/to/firecracker \
  --kernel           /path/to/vmlinux \
  --rootfs           /path/to/rootfs.ext4
# listens on 127.0.0.1:7878 by default
```

Exercise it from another terminal:

```bash
# Create a sandbox (body optional; defaults are 1 vCPU, 512 MiB)
curl -sS -X POST http://127.0.0.1:7878/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"vcpus": 2, "memory_mib": 512}'
# → {"id":"sbx_...","vcpus":2,"memory_mib":512,"workdir":"...","created_at":"..."}

# List all
curl -sS http://127.0.0.1:7878/sandboxes

# Get one
curl -sS http://127.0.0.1:7878/sandboxes/sbx_...

# Tear down
curl -sS -X DELETE http://127.0.0.1:7878/sandboxes/sbx_...
```

Each sandbox gets its own workdir under `--work-base` (default `/tmp/crucible/run/`) containing the Firecracker API socket and `firecracker.log` — the guest kernel + userspace serial console stream into that log file so you can tail it while developing. `Ctrl-C` / `SIGTERM` on the daemon gracefully drains active sandboxes before exiting.

## Development

Build and smoke-test:

```bash
git clone https://github.com/gnana997/crucible
cd crucible
make build
./crucible version
```

Make targets:

```bash
make test    # go test ./...
make race    # with -race
make vet     # go vet
make fmt     # gofmt -s -w .
make lint    # golangci-lint run  (requires golangci-lint installed)
make tidy    # go mod tidy
make clean   # rm built binary
```

Repository layout:

```
cmd/crucible/       CLI entry + subcommand wiring
internal/fcapi/     hand-written Firecracker HTTP-over-UDS client
internal/runner/    firecracker process lifecycle
internal/sandbox/   ID generation + Manager (lifecycle, concurrency-safe)
internal/daemon/    HTTP server, routes, middleware
internal/version/   ldflags-settable build version
docs/               VISION.md + ROADMAP.md
```

Zero external dependencies — all HTTP, JSON, concurrency, and process handling is stdlib. The Firecracker API client is hand-written rather than using the official SDK; see [docs/VISION.md](docs/VISION.md) for the rationale.

CI runs `go vet`, `gofmt` check, `-race` tests, `go build`, and `golangci-lint` on every push and PR.

## Roadmap at a glance

- **v0.1** core runtime: Firecracker orchestration, HTTP API, snapshot/fork, quotas, observability primitives, Python SDK
- **v0.2** policy files, language profiles, custom rootfs builder, DNS filtering
- **v0.3** full OpenTelemetry, syscall tracing, record + replay
- **v0.4** fork trees with pruning and scoring
- **v0.5** multi-tenant mode, API keys, audit log
- **v0.6** Kubernetes operator
- **v0.7** distributed scheduler across a fleet of hosts
- **v0.8** GPU passthrough (likely via Cloud Hypervisor)
- **v0.9** streaming exec + interactive stdin
- **v1.0** stability commitment, security audit, first-party integrations (Claude Code, Cursor, LangChain, MCP)

Details in [docs/ROADMAP.md](docs/ROADMAP.md).

## Contributing

Too early. Nothing functional to contribute to yet. Once v0.1 lands, the "please PR" list will include additional language profiles, per-language seccomp tuning, and integrations with specific agent frameworks.

If you're building a coding agent and want crucible to fit your use case, open an issue with your workflow. Concrete use cases shape the roadmap more than abstract wishlist items.

## License

Apache License 2.0. See [LICENSE](LICENSE).

---

*crucible is a working name. If a better one emerges during v0.1, it'll change before v1.0.*
