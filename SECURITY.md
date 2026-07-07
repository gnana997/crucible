# Security

crucible exists to run **untrusted code** — so isolation is a core design property, not an afterthought. This document describes the isolation model, the honest limitations of the current release, and how to report a vulnerability.

## Isolation model

Each sandbox is a **Firecracker microVM with its own guest kernel** — escape requires breaking out of a virtual machine, not merely a shared-kernel namespace. This is the same isolation primitive AWS Lambda and Fargate use. On top of that boundary, crucible adds:

- **Jailer confinement.** Each microVM's VMM runs inside its own chroot, with its own mount and PID namespaces, and drops to an unprivileged uid before executing. A compromised VMM is contained to its jail.
- **Private base images.** The guest kernel and per-fork snapshot artifacts are staged so that a compromised VMM cannot mutate an image shared with other sandboxes or future forks.
- **Per-sandbox networking, default-deny.** Each networked sandbox lives in its own network namespace with its own address. Egress is denied by default; a sandbox may only reach IP addresses the daemon's DNS proxy resolved for explicitly allowlisted hostnames. Resolved addresses are range-filtered, so a guest cannot reach link-local, RFC1918, or carrier-grade-NAT ranges — closing SSRF paths to cloud metadata endpoints. An ingress filter with per-sandbox source anti-spoofing prevents one sandbox from impersonating another to the daemon.
- **Clone-safety.** When a sandbox is forked from a snapshot, the fork's kernel RNG is reseeded with fresh host entropy and its machine identifiers are rotated **before** the fork can be exec'd — so no two forks wake sharing RNG state, UUIDs, or machine-id. (The kernel's VMGenID mechanism further narrows the reseed window on guest kernels that support it.)
- **Resource ceilings.** Per-request limits on vCPU count, memory, and fork fan-out are enforced at the API boundary to bound the blast radius of a single request. Under jailer, each VMM additionally runs in a cgroup v2 slice whose `cpu.max` / `memory.max` / `pids.max` are sized from the sandbox's own request (on by default; `--cgroup-quotas=off` to disable), so a runaway VMM can't starve the host.

## Agents driving crucible (MCP)

`crucible mcp serve` lets an LLM agent spawn sandboxes and run code as native MCP tools. This shifts the threat model: the Firecracker VM still protects the **host**, but a **prompt-injected** agent (a poisoned page or file it reads) inherits the agent's crucible capability. The MCP guardrails exist to **bound what the agent can do**, on the assumption it may be turned against you.

The operator sets policy at launch (`--net-allow-max`, `--max-sandboxes`, `--max-fork`, `--max-timeout`, `--allow-profiles`, `--tools`/`--deny-tools`); the agent operates strictly within it and cannot widen it. Networking stays default-deny, and even an agent-chosen allowlist is range-filtered, so agent egress reaches only *public* hosts it names — never cloud-metadata or internal services.

**The local same-user bypass — closed for scoped tokens.** A local daemon key sits in a file your OS user can read, so a same-user agent with a shell tool could read it and call the loopback daemon directly, past the MCP guardrails. **Scoped tokens** close this: the daemon enforces each token's policy (operations, network, resource caps, expiry), so a stolen scoped token buys only the bounded capability it already had — the bypass gains nothing. Scope the keys you hand to agents; an *unscoped* key still grants full access, so protect it like any credential. See [docs/policy.md](docs/policy.md) for the policy model and [docs/mcp.md](docs/mcp.md#limitations).

## Current status and limitations

crucible is **pre-1.0 and is not yet hardened for production or for untrusted multi-tenant use.** Please treat the following as known, deliberate limitations of a pre-release:

- **Authentication.** The daemon supports bearer-token API keys (`crucible daemon token add`), stored as SHA-256 hashes; once any key exists every request must present it, and binding a non-loopback address is refused unless keys **and** TLS are configured. Keys can be **scoped** to a daemon-enforced policy (allowed operations, egress ceiling, profile allowlist, resource caps, expiry — see [docs/policy.md](docs/policy.md)), or left unscoped for full access. The daemon still **binds `127.0.0.1` by default**; treat any exposed instance with an unscoped key as granting full code execution on the host's behalf.
- **Single-host, single-operator.** crucible assumes a trusted operator running it on their own host. It has not been validated for hosting mutually-distrusting tenants.
- **No audit trail** beyond operational logs (per-sandbox activity logs are planned).

We aim to be production-honest: pre-1.0 means pre-1.0. Do not rely on crucible as a security boundary for untrusted, adversarial, multi-tenant workloads until a release explicitly commits to that, backed by an external review.

## Supported versions

crucible is pre-release software under active development. Only the latest `main` receives fixes; there is no long-term-support commitment yet. Security guarantees will be stated explicitly at the `v1.0` milestone.

## Reporting a vulnerability

**Please report security issues privately — do not open a public GitHub issue.**

- Open a private report via GitHub's **"Report a vulnerability"** (Security → Advisories) on this repository. This is the preferred and supported channel.

Please include a description of the issue, the affected version/commit, and — if possible — reproduction steps or a proof of concept. We'll acknowledge receipt and work with you on a fix and coordinated disclosure. Given the pre-release status, there is no formal SLA yet, but security reports are taken seriously and prioritized.
