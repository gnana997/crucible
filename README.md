# crucible

> Sandbox runtime for AI coding agents. Firecracker microVMs, a single Go binary, snapshot/fork as first-class primitives.

![Status: scaffolding](https://img.shields.io/badge/status-scaffolding-red)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue)
![Core: Go](https://img.shields.io/badge/core-Go-00ADD8)

> **Heads up.** This repo is early scaffolding. The `crucible` binary builds, prints help, and exits. No sandboxing primitives are implemented yet. Come back when v0.1 ships, or follow along as it's built.

## Why this exists

AI coding agents write code and they want to run that code — to check compile, run tests, try three approaches in parallel. The options today are all wrong in different ways: raw Docker (shared kernel, weak isolation, no fork), hosted sandbox services (lock-in, cost scales with usage), or rolling your own Firecracker stack (months of operational work).

crucible is the fourth option: a single self-hosted Go binary on top of Firecracker, with snapshot/fork as first-class primitives, sane quotas and observability baked in, defaults tuned for AI-generated code.

Full motivation, design, and FAQ: [docs/VISION.md](docs/VISION.md).

## Status

| Capability | Status |
|---|---|
| Go module + CLI skeleton | ✅ done (scaffolding) |
| Firecracker runner (boot VM from config) | 🔨 in progress |
| HTTP API — create / exec / delete | ⏳ planned |
| Resource quotas | ⏳ planned |
| Snapshot + fork primitives | ⏳ planned |
| Default-deny network + allowlist | ⏳ planned |
| Structured execution record per exec | ⏳ planned |
| Prometheus `/metrics` + JSON logs | ⏳ planned |
| Python SDK | ⏳ planned |
| Install script + systemd unit | ⏳ planned |

Full trajectory through v1.0: [docs/ROADMAP.md](docs/ROADMAP.md).

## Development

Requirements:

- Linux host with KVM (x86_64). Run `ls /dev/kvm` — if it exists and is readable, you're good.
- Go 1.24+
- For actually running sandboxes (not yet wired up): Firecracker v1.15+ installed on the host.

Build and smoke-test:

```bash
git clone https://github.com/gnana997/crucible
cd crucible
make build
./crucible version
```

Other Make targets:

```bash
make test    # go test ./...
make race    # with -race
make vet     # go vet
make fmt     # gofmt -s -w .
make lint    # golangci-lint run  (requires golangci-lint installed)
make tidy    # go mod tidy
make clean   # rm built binary
```

CI runs vet, gofmt check, race tests, build, and golangci-lint on every push and PR.

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
