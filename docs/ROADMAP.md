---
title: Roadmap
description: "What has shipped and what is planned, in rough order, grouped into coherent milestones."
---

# Roadmap

What's shipped and what's planned, roughly in order. Each section is a coherent milestone, items inside a section are the ones that landed (or are expected to land) together. A `[x]` is actually working on `main`; a `•` is planned. Nothing is called "done" until it ships.

For the motivation and design principles behind these choices, see [VISION.md](VISION.md).

---

## Shipped

### v0.1: Core runtime

The minimal usable thing: boot a sandbox, run a command inside it, get a structured result back, and fork it cheaply.

- [x] Firecracker orchestration in Go (boot a microVM via the Firecracker API), under the jailer (chroot + mount/PID namespaces + privilege drop)
- [x] HTTP API: create / list / inspect / delete sandboxes, exec, snapshot, fork
- [x] Snapshot + fork primitives, a fork serves guest memory lazily from the snapshot via `userfaultfd` (no per-fork RAM copy)
- [x] Clone-safety, per-fork kernel RNG reseed and machine-identifier rotation, applied before a fork is reachable
- [x] Default-deny networking, each sandbox in its own netns, egress limited to a DNS-proxy hostname allowlist with range-filtered resolved addresses
- [x] Per-request resource ceilings (vCPU, memory, fork fan-out) plus a per-sandbox lifetime and a per-exec deadline
- [x] Structured execution record per exec (exit code, timing, signal, timeout/OOM flags, and CPU/memory/page-fault/context-switch/IO usage)
- [x] Durable sandbox registry with reconcile-on-restart, and structured JSON lifecycle logs
- [x] Host-side cgroup quotas (cpu.max / memory.max / pids.max) sized per sandbox, on by default under jailer
- [x] Prometheus `/metrics` endpoint, `sandboxes_created_total`, `sandboxes_active`, `fork_duration_seconds`, `snapshot_restore_duration_seconds`
- [x] Native language rootfs profiles (`base`, `python`, `node`, `go`, versioned), built from official language images via `make profile` ([profiles.md](profiles.md))
- [x] CLI over the REST API, `sandbox`, `snapshot`, `fork`, `profile ls`, and a `run` one-shot, on a reusable typed Go client; `-o json` everywhere ([cli.md](cli.md))
- [x] Install script + systemd unit, `install.sh` drops the binary, a `crucible.service` unit, and a config template

### v0.1.2: MCP server + API-key auth

- [x] **MCP server** (`crucible mcp serve`), a stdio [Model Context Protocol](https://modelcontextprotocol.io) server so any MCP agent (Claude Code, Cursor, …) drives crucible as native tools, with operator guardrails the agent can't widen. Built on the `sdk` Go package, so an MCP call and the CLI hit the identical path and can't drift ([mcp.md](mcp.md)).
- [x] **Daemon API-key auth**: bearer keys hashed at rest; once any key exists every request must present it, and binding a non-loopback address is refused without keys **and** TLS ([SECURITY.md](../SECURITY.md)).

### v0.1.3: Scoped / policy tokens

- [x] Bind a key to a policy the **daemon** enforces (allowed operations, an egress ceiling, a profile allowlist, resource caps, and an expiry) so a handed-out key is worthless beyond its bounds. `crucible policy validate/show`, `GET /whoami` ([policy.md](policy.md)).

### v0.2.0: TUI + fork lineage

- [x] **TUI** (`crucible tui`), a live terminal dashboard: running sandboxes, the fork tree, and interactive streaming `exec`, with create/snapshot/fork/delete gated on the token's scope. A thin consumer of the `sdk` Go package ([tui.md](tui.md)).
- [x] **Fork lineage on the API**: `source_snapshot_id` records which snapshot a sandbox was forked from, so the fork genealogy is reconstructable by any client (this is what the tree view draws).

### v0.3.x: The safe `docker run` for untrusted/AI code

- [x] **OCI image boot**: `crucible run <image>` boots an unmodified image's entrypoint in a microVM; `crucible build` builds a Dockerfile and loads it into the store (daemon stays Docker-free). Publish host ports with `-p`.
- [x] **Interactive shell**: `crucible shell <id>` / `sandbox exec -i`: a real long-lived `/bin/sh` over a hijacked full-duplex vsock stream (state persists; line-buffered, **no PTY**). The TUI gains a **scrollback + `tab`-to-shell** session view.
- [x] **`--disk`** per-sandbox writable sizing (`resize2fs` the clone, never the shared image); top-level **`stop`/`rm`** ops verbs; durable **`logs`**.
- [x] **MCP for the wedge**: `image`/`pull`/`publish`/`disk_mib` on `create_sandbox`/`run`, plus `logs` and `stop_sandbox` tools ([mcp.md](mcp.md)).
- [x] **Complete orphan reaping**: startup sweeps live orphan processes and empty orphan cgroups; a killed daemon leaves no lingering firecracker.
- [x] **Copy files in / out**: `crucible cp <local> <sbx>:<path>` (and back out), tar over vsock, the safe-*copy* model (not a host bind-mount), tar-slip-safe and size-bounded on the way out; plus MCP `write_files`/`read_file`. Drop code in and run it, no image build.

### v0.4.0: Durable, self-healing apps

- [x] **Durable app model**: `crucible app create <name> --image …` promotes a workload to a named **app** the daemon keeps a healthy instance of. Desired state lives in a bbolt control-plane store; the ephemeral `sandbox` primitive is unchanged ([apps.md](apps.md)).
- [x] **Survives restart**: the app reconciler re-creates each app's instance from spec after a daemon restart or host reboot (desired-state reconcile, the Fly/k8s model, *re-created*, not live-re-attached; in-VM memory is lost, cost is one cold boot).
- [x] **Self-heal**: daemon-side restart-on-failure with **exponential backoff + a crash-loop guard**, plus **http/tcp health checks** (declarative `always`/`on-failure`/`never` policy).
- [x] **Full surface**: `crucible app ls|get|rm|logs|exec|shell`, REST `/apps`, the Go SDK (`CreateApp`/`ListApps`/`GetApp`/`DeleteApp` + an `App` handle), and four MCP tools (`create_app`/`list_apps`/`get_app`/`delete_app`, → 19 tools).
- [x] **`crucible fork -p HOST:GUEST`**: publish a host port on a fork (a running server, forked and exposed on its own port).

- **Durability contract:** an **app** survives a daemon restart (re-created from desired state); a bare **sandbox** does not (it stays the throwaway primitive). Live-VM re-attach (avoiding the cold boot) is later trajectory work.

### v0.4.1: Apps you can actually deploy

Turn a durable app from *survivable* into *deployable*: real config and real egress, across `app create`, `run`, and `sandbox create`.

- [x] **App env**: `-e/--env KEY=VALUE` delivered to the entrypoint (image `ENV` < your `--env`).
- [x] **Real egress**: `--net-full-egress` (reach any public host) and `--net-allow-cidr 203.0.113.0/24` (public IP literals) for a workload you deploy yourself. **Public-hosts-only, no exceptions**: metadata/link-local/RFC1918/CGNAT/reserved are always dropped (the nft guard is unit-tested to agree with the DNS-layer SSRF filter), gated by a `net_full_egress` scoped-token grant.
- [x] **Exec health checks**: `--health-cmd '<command>'` runs a command in the guest (exit 0 = healthy), joining http/tcp.
- [x] **Publish declared ports**: `-P/--publish-all` publishes every port the image `EXPOSE`s (guest N → host N).

### v0.4.2: Reach it by name

The durable app is now reachable by name and updatable in place.

- [x] **Ingress proxy**: reach an app by name instead of a published port. `--proxy-listen` (Host-header routing, L7), `--proxy-tls-listen` (SNI passthrough, L4, the guest terminates its own TLS), `--proxy-domain <domain>` (`web.<domain>` → app `web`). Off in the daemon by default; the installer enables it on `:7879` (next to the API, clear of the `:80`/`:8080`/… ports you publish to), overridable to `:80`/`:443` for a production ingress. Resolution is live, so the route follows the app across self-heal and redeploy and never points at a stale IP ([proxy.md](proxy.md)).
- [x] **`crucible app update`**: replace an app's spec (name immutable) and redeploy: the reconciler bumps the generation, destroys the old instance, and boots a fresh one. Also on the Go SDK (`UpdateApp`) and the MCP `update_app` tool. *(v0.4.3 makes this zero-downtime, see below.)*
- [x] **Health seeded from the image**: an app that declares no health inherits the image's Docker `HEALTHCHECK` (as an `exec` check) when present.
- [x] **Inbound isolation**: inbound reaches a guest only from the daemon over its veth; peers can't reach each other and a guest can't reach the proxy listeners at all, so the proxy is not a lateral path.

### v0.4.3: Operate & safe-update

Update a deployed app without dropping traffic, and drive a running app by name.

- [x] **Zero-downtime rolling `app update`**: for a proxy-fronted app the reconciler boots the new instance, waits for a **readiness gate** (its health check, or a TCP connect to the app's port), **flips the ingress route** to it, then drains the old instance before destroying it. The proxy follows the flip, so the cutover drops nothing. A **failed** update aborts and keeps the old instance serving (never takes the app down); `status.instance_generation` shows which spec is live.
- [x] **Operate an app by name**: `crucible app exec`/`logs`/`shell` (and MCP `app_exec`/`app_logs`) resolve the app's **current** instance server-side per call, so they survive a self-heal or redeploy. `app logs -f` reattaches to the new instance across a roll. Flag parity added (`app exec --cwd/--timeout/-e`, `app shell --shell`).

### v0.4.4: Private registries

Pull authenticated images, with the credential on the daemon.

- [x] **Private-registry pull**: `crucible registry login/logout/ls` stores a per-registry credential on the daemon (`--registry-store`, `0600`, gated by a new `registry` scoped-token op) that feeds every pull: `run`, `app create`, and an app's **re-pull on restart** (so a durable app on a private image survives a reboot). Static creds cover Docker Hub, GHCR, GitLab, Quay, self-hosted, and static GCP/ACR; ECR via `aws ecr get-login-password`. Never reads `~/.docker/config.json`. Plus one-shot `run --registry-auth` for CI ([registry.md](registry.md)).

### v0.5.0: Scale to zero

An app sleeps when idle and wakes on the next request in under a second (same IP, same identity, correct clock) and survives a daemon restart while asleep. Built by re-pointing machinery crucible already ships (snapshot/restore with lazy memory, clone-safety, the reconciler, the ingress proxy) at a new policy, not a new subsystem.

- [x] **App sleep/wake**: `crucible app sleep`/`app wake` snapshot a running app and stop its VMM to free RAM + CPU while keeping the netns, subnet/IP reservation, and ingress route, then restore it **in place**: same instance id, same IP (no DHCP bounce), CRNG reseeded and clock stepped to host time before it's reachable, but (unlike a fork) machine-id/hostname *not* rotated. On the Go SDK (`SleepApp`/`WakeApp`) and MCP (`app_sleep`/`app_wake`) ([apps.md](apps.md)).
- [x] **Automatic scale-to-zero**: `app create --idle-timeout <dur> --min-scale 0` drops an idle app to ~zero RAM: the ingress proxy tracks per-app last-activity + open connections and, once idle and healthy, the reconciler sleeps it. The next request **triggers a wake, holds the request, and forwards it once the app passes readiness**: a request herd coalesces into a single wake. `--min-scale ≥1` stays always-warm; `--idle-timeout 0` never sleeps.
- [x] **Survives a daemon restart while asleep**: sleep captures a durable snapshot (journaled record + cloned rootfs), so a slept app is re-adopted on daemon start and the first post-restart request wakes a fresh instance from it.
- [x] **Wake admission gate**: a wake is refused (`503`, app stays asleep) when host free memory is below `--wake-min-free-mib` (default 256), plus `snapshots_active` and an `app_wake_latency_seconds` histogram on `/metrics`.

### v0.5.1: App→app networking

Deploy your frontend and API as separate apps and let them talk, the first step from "run an app" to "deploy a system." Stateless service-to-service, so it doesn't wait on volumes.

- [x] **Reach another app by name**: with the daemon's `--internal-networking`, an app calls another at `http://<app>.internal/`, routed **through the ingress proxy VIP** (the DNS anycast) to the callee's current instance. Because it goes through the proxy (not a direct guest-to-guest path) an internal call inherits **wake-on-request** (a scaled-to-zero callee wakes and serves) and leaves per-sandbox isolation intact (a guest still can't reach a peer's IP directly) ([apps.md](apps.md)).
- [x] **Default-deny authorization**: `app create <app> --can-call <other>` declares the calls an app may make (empty = none). Enforced daemon-side: the proxy returns 403 on an un-granted call, and DNS answers `<app>.internal` only for granted callers (else NXDOMAIN, no discovery of apps you can't call). On the Go SDK (`AppSpec.CanCall`) and MCP; `app_internal_requests_total` on `/metrics`. Experimental, off by default.

### v0.5.2: Scale out

An app runs multiple replicas behind the proxy, load-balanced, and autoscales on request concurrency, each replica forked **warm** from a snapshot in milliseconds, not cold-booted. "k8s horizontal scaling, but the VM properties invert the tradeoffs."

- [x] **Horizontal scaling**: `app create <app> --min-scale N` runs N warm replicas, each stamped by **forking a golden snapshot** of the healthy primary (lazy memory; clone-safe, a distinct machine-id and IP per replica). The reconciler self-heals the fleet: a dead replica is replaced ([apps.md](apps.md#horizontal-scale-out)).
- [x] **Load balancing**: the proxy balances requests across an app's live instances with **power-of-two-choices least-request**, a slow-start ramp so a just-forked replica isn't slammed cold, and passive outlier ejection. External and app→app traffic both balance through the one path.
- [x] **Autoscaling**: `--max-scale M --target-concurrency C` autoscales between the floor and M on concurrency: a fast window scales up on bursts, a slow window scales down when calm (stabilized against flapping). `min_scale=0` composes with scale-to-zero (idle→0, request→1, load→M). `app ls` shows a replicas column.

### v0.5.3: Reliability & isolation hardening

Close the sharp edges the scale-out and app→app work surfaced: no leaked VMs, no stale agents, and no port contention between publishing and internal networking.

- [x] **No orphaned instances**: teardown destroys every instance an app owns (current + draining + incoming), a superseding update reaps the prior draining instance, and sleep frees in-flight roll instances. A rolling update's old VM is always reaped, even on delete / re-update / sleep mid-drain ([smoke_leaks.sh](../scripts/smoke_leaks.sh)).
- [x] **Agent-fresh image cache**: converted OCI images are keyed by the injected agent's digest, so a daemon upgrade re-converts instead of booting a stale baked agent (the cause of `wake` failing on an image an older daemon cached).
- [x] **Publish coexists with app→app**: the `<app>.internal` VIP and a published host port on the same number no longer clash (`SO_REUSEPORT` + a host-port registry that preserves one-owner-per-port).

### v0.5.4: Observability

Make a running app legible (per-app metrics, OTLP export of metrics and logs, daemon profiling, and on-demand packet capture) while keeping the in-daemon surface small: crucible emits open standards and delegates routing to your collector.

- [x] **Per-app metrics**: request rate / latency / status class from the proxy + lifecycle gauges (`app_replicas`/`ready`/`up`/`asleep`/`sleep_total`/`wake_latency`) on `/metrics`, cardinality-bounded, with a reference Grafana dashboard ([observability.md](observability.md)).
- [x] **OTLP export (metrics + logs)**: one `--otlp-endpoint` flag pushes the same `/metrics` series (via an OpenTelemetry Prometheus bridge, no metric redefinition) and streams app logs from the durable log store; honors `OTEL_EXPORTER_OTLP_*` env. Point it at any collector.
- [x] **Daemon pprof**: `--pprof-listen` serves Go `net/http/pprof` (off by default; loopback-guarded).
- [x] **On-demand packet capture**: `sandbox capture` / `app capture` streams host-side pcap (no in-guest tcpdump; distroless-safe), gated by a default-deny `capture` scoped op and audited; MCP `capture`/`list_images`/`delete_image` tools (24→27).

### v0.6.0: Persistent volumes

Data that outlives the sandbox. A named, fsync-honest block device you attach to a database's data directory, a browser profile, or an upload folder, surviving destroy/re-create, a hard VM kill, an app redeploy, sleep, and a daemon restart. Stateless workloads keep the snapshot/fork magic; stateful ones trade it for single-writer correctness ([volumes.md](volumes.md)).

- [x] **Volumes on sandboxes**: `run --volume NAME:/path` attaches a durable ext4 block device (created + formatted on first use, reattached by name), backed by a sparse file under the daemon's `--volume-dir` and mounted `cache_type=Writeback` so a guest `fsync` reaches the host: committed data survives a hard kill of the VM.
- [x] **Volume lifecycle**: `volume create --size` / `ls` / `rm` (rm refused while attached), a durable bbolt record store that survives restarts, REST `/volumes`, and MCP `volume_create` / `list_volumes` / `delete_volume` (27→30 tools).
- [x] **Volume-backed apps**: `app create --volume`; single-writer, so redeploy is destroy-then-boot (not the zero-downtime flip) and sleep is stop/start (quiesce → destroy → cold-create on wake, not a snapshot). Data survives `app update`, sleep, and daemon restarts.

### v0.6.1: Wake-on-TCP (serverless)

Scale-to-zero for **any** self-hosted TCP service: databases, caches, brokers. A scale-to-zero app's published port is fronted by an L4 forwarder that wakes the app on the first connection and sleeps it when idle, with no ingress proxy in the path, protocol-agnostic. On a volume, that's a **self-hosted serverless postgres or redis** ([serverless.md](serverless.md)).

- [x] **Wake-on-TCP:** a scale-to-zero app (`--min-scale 0 --idle-timeout`) that publishes a host port (`-p`) gets an app-scoped waking forwarder: it owns the port for the app's life (surviving its sleep), resolves the current instance per connection, wakes a slept app on connect (a burst coalesces into one wake), and forwards. TCP activity feeds the idle monitor, so it sleeps on inactivity. Automatic: no new daemon flag, no proxy required. Works for postgres, mysql, redis, mongo, or a custom daemon.
- [x] **Idle-connection reaping** (`--connection-idle-timeout`, default = `--idle-timeout`): the forwarder closes a byte-idle connection so a pooled client (which holds connections between queries) still lets the app reach zero connections and sleep; the pool reconnects on its next query.
- [x] **Connection-scoped mode** (`--keep-connections`): reaping off with TCP keepalive, so the app stays awake while any client is connected and sleeps only when the last disconnects. The mode for pub/sub, `LISTEN/NOTIFY`, and streaming.
- [x] **Serverless postgres & redis:** a volume-backed database sleeps (stop/start, VM freed) and cold-wakes on the next connection with data intact; a redis scales to zero between requests or stays awake for a live subscriber.
- [x] **Guest `/dev/fd`:** the init now creates the standard `/dev/fd` + `/dev/std{in,out,err}` → `/proc/self/fd` symlinks, so bash process substitution `<(…)` works (postgres's password init and other entrypoints depend on it).

### v0.6.2: Instant serverless-stateful

A volume-backed app used to cold-boot on wake (sleep destroyed the instance; wake booted a fresh one and the database ran recovery). Now it snapshot-sleeps and snapshot-wakes like a stateless app, so a serverless postgres comes back in about 170 ms with **no cold boot and no WAL recovery** ([serverless.md](serverless.md)).

- [x] **Snapshot-sleep + wake-in-place for volume apps:** sleep snapshots the instance and stops the VMM (RAM freed, single-writer guard held); wake restores it in place with the volume re-attached, same instance and IP, ~170 ms (reflink), data intact. The process is already running in the restored memory.
- [x] **Fast wake after a daemon restart:** a slept volume app re-adopts as asleep and wakes from its durable snapshot into a fresh instance (new IP, still no cold boot), re-acquiring the volume guard.
- [x] **Durable-while-asleep fsync:** the volume backing file is fsync'd host-side before the VMM stops (Firecracker does not flush drive backing files on snapshot), so a host crash while asleep cannot lose committed rows.
- [x] **Automatic cold-boot fallback:** a snapshot-restore failure falls back to stop/start cold-create, so a wake never fails.

### v0.9.2: Cold app stop/start *(current)*

A desired-state stop/start, and the recipe to grow a running app's volume ([apps.md](apps.md#the-crucible-app-commands)).

- [x] **`crucible app stop <name>` / `app start <name>`:** a cold desired-state stop (destroy the instance, **detach the volume**, retain the spec) and start (boot a fresh instance, re-attach at the current size), distinct from `sleep`/`wake` (which snapshot and hold the single-writer guard). `stop` returns once the instance has torn down. `POST /apps/{name}/stop|start`, SDK `StopApp`/`StartApp`, MCP `app_stop`/`app_start`.
- [x] **The grow recipe:** `app stop → volume grow → app start` grows a running app's volume (grow itself stays detached-only). `scripts/smoke_volume_grow.sh` exercises it end-to-end (stop, grow, start, data intact, guest sees the new size).
- Next: incremental backups.

### v0.9.1: Grow a volume

Enlarge a volume in place instead of backup → restore-to-bigger → redeploy ([volumes.md](volumes.md#growing-a-volume)).

- [x] **`volume grow <name> --size <newsize>`:** enlarges a volume's backing store and its ext4 filesystem to the new total size — grow-only (a size at or below the current one is refused; ext4 can't shrink online). Encrypted volumes grow too: the LUKS container, its mapping, and the ext4 are all resized in one step. `POST /volumes/{name}/grow` (gated by `create`), SDK `GrowVolume`, OpenAPI `growVolume`; the shared `resize2fs` mechanism runs `e2fsck` first so a volume detached from a hard-killed guest resizes cleanly.
- [x] **Detached-only, safe by construction:** a `409` while attached. A snapshot-slept volume's guest has its block-device size pinned by the snapshot, so an offline grow only takes effect on the next fresh boot — growing while detached and restarting sidesteps that. `scripts/smoke_volume_grow.sh` grows a plaintext + an encrypted volume, confirms the guest reports the new capacity with data intact, and that shrink + attached-grow are refused.

### v0.9.0: Guest metrics scrape

A workload's own metrics — a database's `pg_stat_*` / Redis `INFO`, or any app's Prometheus endpoint — folded into the daemon's `/metrics` ([observability.md](observability.md#guest-metrics--scrape-an-apps-metrics)).

- [x] **Scrape a guest `/metrics`:** `app create <name> --metrics-port <p> [--metrics-path]` points the daemon at a Prometheus endpoint inside the guest (a `postgres_exporter`, `redis_exporter`, or the app itself); the daemon scrapes it on `--guest-scrape-interval` and re-exposes the series on its own `/metrics` + OTLP with `app`/`instance` labels. DB-agnostic (it federates the text; the exporter is a guest process). SDK + MCP.
- [x] **Scale-to-zero aware:** a slept / non-routable instance is never scraped and a scrape never wakes it (direct-dial, not the wake forwarder); `crucible_guest_scrape_up` drops to 0 and the series drop out while asleep. Every scrape is body/series/timeout-capped so a guest can't flood the daemon; a malformed family can't 500 the endpoint. `scripts/smoke_guest_scrape.sh` proves the series land with labels and that sleeping stops the scrape without waking the app.
- The daemon ships only the generic scrape; wait-event / average-active-sessions sampling (the Performance-Insights core) is a guest-side exporter concern the scrape ingests unchanged. Dashboards / query analysis are downstream.

### v0.8.1: Encryption key management

Multiple keys, and key rotation that re-encrypts no data ([encryption.md](encryption.md#multiple-keys--rotation)).

- [x] **Keyring:** more than one encryption key — additional keys from `CRUCIBLE_VOLUME_KEY_<ID>` env (off-disk) and `<id>.key` files under `--volume-key-dir`, alongside the default key. `--volume-default-key <id>` wraps new volumes; `volume ls` shows each volume's `KEY`.
- [x] **Rotate without re-encrypting data:** `volume rewrap <name> --to-key <id>` (+ `--all --from-key <id>`) re-wraps the per-volume key under a different key — the LUKS volume key never changes, so no `cryptsetup`, no data movement, no downtime, safe on a live volume. `POST /volumes/{name}/rewrap` + `/volumes/rewrap`, gated by a new default-deny `volume_key` op; SDK + CLI.
- [x] **Reload + retire:** `volume keys reload` swaps the keyring in without a restart (refuses to drop a key a volume still uses; a missing key fails to open with a clear error). Every key op — created / rotated / shredded — is audit-logged with names + key ids only, never key material. `scripts/smoke_key_rotation.sh` rotates a running app's volume, retires the old key, and confirms the data survives + no key material hits the log.

### v0.8.0: Encryption at rest

A persistent volume's data — encrypted on disk with a per-volume key you can destroy ([encryption.md](encryption.md)).

- [x] **Per-volume encryption:** with a master key (`--volume-encrypt-key-file` / `CRUCIBLE_VOLUME_KEY`), a volume is a LUKS2 container (`aes-xts-plain64`, AES-256-XTS) over its backing file, unlocked by a fresh random per-volume key sealed under the master key (AES-256-GCM, volume name as AAD) and stored in the record — never in the clear. `crucible volume create --encrypt` / `--no-encrypt`, `--volume-encrypt` daemon default, an `ENCRYPTED` column on `volume ls`, `CreateVolumeRequest.encrypt` + `Volume.encrypted` in the API/SDKs.
- [x] **Transparent to the guest:** on attach the daemon opens the container to a decrypted device and stages that device node into the VM's chroot under the jailer (the `/dev/kvm` mechanism), never the ciphertext file. Encryption is in the kernel device-mapper layer — transparent, including to the snapshot/wake pager (no per-page cost); a fast keyslot KDF keeps attach and scale-to-zero wake sub-second. Closed on sleep and re-opened on wake, so a slept database is ciphertext at rest.
- [x] **Crypto-shred:** `crucible volume shred <name>` (`POST /volumes/{name}/shred`, `delete`-gated) destroys the keyslots + wrapped key so the data is permanently unrecoverable — refused while attached and on plaintext volumes. Backups carry the wrapped key; `volume restore` re-wraps it under the new name. `scripts/smoke_volume_encrypt.sh` runs a real Postgres and proves the on-disk container is ciphertext (including while asleep), backup→restore round-trips the data, and shred makes it unrecoverable.
- Protects a stolen/seized disk (the AWS-EBS model), **not** a compromised host root — confidential computing (encrypted guest RAM) is unsupported by Firecracker and incompatible with lazy-paging wake. `volume clone` of an encrypted volume and snapshot-memory encryption are not yet done (encrypt `--work-base` for the latter).

### v0.7.4: Secrets

Sensitive config out of the cleartext app spec and into an encrypted store ([secrets.md](secrets.md)).

- [x] **Encrypted secret bundles:** a secret is a named `key→value` bundle (a `.env` becomes one bundle) sealed AES-256-GCM (bundle name as AEAD AAD) in `internal/secretstore`. Write-only management — `crucible secret set --from-env-file`/`ls`/`rm`, `PUT/GET/DELETE /secrets/{name}` gated by the default-deny `secret` op; no endpoint ever returns a value. Injected with **envFrom** (`app create --secrets <bundle>` / `--secrets-from .env`) at boot, so the app spec/backups carry only the bundle name. Opt-in master key (`--secrets-key-file` / `CRUCIBLE_SECRETS_KEY`; no key ⇒ disabled); the store rides `admin backup` as ciphertext, the key excluded. `scripts/smoke_secrets.sh` proves the secret reaches the guest service env while no host surface (app get / API / backup / on-disk store) leaks the value. Snapshot residency is the honest runtime limit — v0.8.0 encrypts volume data at rest, and an encrypted `--work-base` covers the snapshot memory file.

### v0.7.3: App lifecycle events

A stream of app transitions — the activity timeline, and the exact sleep/wake timing that makes awake-interval usage accounting precise ([observability.md](observability.md#app-lifecycle-events--get-events)).

- [x] **App lifecycle event stream:** `created` / `phase_changed` (booted/slept/woke/crashed, carrying `from`/`to` + `wake_latency_ms`) / `health_changed` / `domain_*` / `deleted`, on `GET /events?since=&app=` (cursor-follow, `read`-gated), `crucible events -f` / `crucible app events`, and as OTLP log records. In-memory ring (`--events-buffer`, default 1024); a slow consumer never back-pressures an app (drop-on-full). Emitted via an `emitPhase` de-dup helper — exact sleep/wake boundaries emit directly, a reconcile-pass sweep catches the rest, so no net phase change is missed and none is emitted twice. `scripts/smoke_events.sh` drives create→boot→sleep→wake→update→delete and asserts the ordered, monotonic stream + a `?since` resume.

### v0.7.2: Egress bytes

The fifth usage dimension, completing the ledger ([observability.md](observability.md#persistent-usage-metrics)).

- [x] **Per-app egress bytes:** cumulative external outbound bytes joins the durable usage ledger (`crucible app usage`, `GET /usage`, `app_usage_egress_bytes_total`). A dedicated nftables accounting chain (forward hook, priority 10 — past the filter chain's `ct established` short-circuit and after its accept, so it counts every packet of actual accepted egress) holds a named per-sandbox counter; the ledger folds it in as a per-instance delta so a redeploy (which resets the counter) neither loses nor double-counts. Outbound + external only — downloads and intra-host DNS / app→app traffic are excluded by construction. `scripts/smoke_usage.sh` gains an egress step (external I/O counted + attributed, idle app stays ~0).

### v0.7.1: Usage metrics & cert status

See what an app has used, and whether its domains are certified ([observability.md](observability.md#persistent-usage-metrics)).

- [x] **Persistent usage metrics:** a durable, cumulative per-app ledger (compute vCPU-seconds while awake, memory MiB-seconds while awake, storage GiB-seconds, requests) persisted with the app records, so it survives a daemon restart. Read via `crucible app usage`, `GET /usage` + `/apps/{name}/usage` (`read`-gated), and `app_usage_*` Prometheus/OTLP counters. A slept app accrues no compute; a restart doesn't back-fill downtime; a deleted app's final usage is retained. Accrual is checkpointed on a `--usage-interval` tick (default 60s) + each lifecycle transition. `scripts/smoke_usage.sh` proves compute freezes while asleep and survives a restart.
- [x] **TLS certificate status:** per-domain state (active / expiring / pending / failed / manual / passthrough) on `crucible app domain ls`, `GET /apps/{name}/domains?detail=1`, and `app_cert_state` / `app_cert_not_after_seconds` metrics — so a mis-pointed domain (failed issuance) is visible, not silent.

### v0.7.0: TLS termination & custom domains

Real HTTPS deploys: the ingress proxy terminates TLS with a certificate it issues and renews automatically over ACME, on generated and custom domains ([tls.md](tls.md)).

- [x] **TLS termination at the proxy:** with `--proxy-tls-listen` open and `--acme-email` (or `--cert-dir`) set, `:443` terminates TLS with a managed cert and reverse-proxies plain HTTP to the guest; with neither, it stays SNI-passthrough as before. Per app, `--tls-mode terminate` (default) / `passthrough`.
- [x] **Automatic HTTPS over ACME:** on-demand issuance + background renewal (CertMagic). `--acme-ca production|staging`, `--acme-ca-url` / `--acme-ca-root` for a private/test CA, storage under `--cert-dir`. HTTP-01 and TLS-ALPN-01 both answered; `:80` serves the challenge and 301-redirects to HTTPS (`--no-https-redirect` opts out).
- [x] **Custom domains:** `crucible app domain add|rm|ls <app> [<domain>]` attaches a globally-unique domain that the proxy routes and certifies like the generated name (MCP `app_domain_*`; SDK `AddDomain`/`RemoveDomain`/`ListDomains`).
- [x] **Issuance gated to app domains:** a cert is only obtained for a name that maps to a live terminate-mode app, so a stray SNI can't burn a cert or the CA's rate limits. Manual certs load from `<cert-dir>/manual/`. `scripts/smoke_tls.sh` drives issuance end-to-end against a local ACME CA (Pebble).

### v0.6.6: Off-host backups

Let a backup leave the host so it survives host/disk loss — without any cloud SDK or credential in the daemon ([backups.md](backups.md)).

- [x] **Export / import:** `volume backup export <id>` streams a backup's bytes off the host (gzip by default; the sparse image's holes compress away), and `volume backup import --source <vol>` streams one back onto a fresh host, after which `volume restore` materializes a volume. A control plane (or a cron script) ships the bytes to an object store; the daemon stays provider-agnostic.
- [x] **`volume_backup` op:** a new default-deny scoped-token op gates export/import (they move volume data), distinct from snapshot-grade backup creation. No MCP tools. `scripts/smoke_offhost_backup.sh` runs the full export → import → restore round trip.

### v0.6.5: Capacity guards

Prove the wake herd and guard the disk, so density (more sleeping apps than host RAM) is safe to charge for ([apps.md](apps.md), [benchmarks.md](benchmarks.md)).

- [x] **Sleep disk floor:** `--sleep-min-free-disk-mib` (default 1024) refuses a sleep when free disk under `--work-base` is low — a snapshot writes a full guest-RAM-sized memory file, so a fleet snapshotting to a full disk would fill it. The app stays running (safe degraded state). The disk complement to the `--wake-min-free-mib` RAM floor; both fail-open.
- [x] **Mass-wake load test:** `scripts/bench_masswake.sh` sleeps a fleet with `app sleep --all` and fires N concurrent wakes, reporting the wake-latency distribution and how gracefully the RAM floor defers to `503` + retry. Measured: 20 concurrent wakes, p99 ~430 ms, with RAM barely moving — lazy paging faults in only each guest's working set, so "everything wakes at once" is safe by construction.

### v0.6.4: Operate with confidence

Upgrade the daemon without dropping apps, back up the control plane in one command, and see scale-to-zero's disk cost — the operability layer under the platform ([upgrades.md](upgrades.md), [backups.md](backups.md)).

- [x] **Upgrade without drop:** `app sleep --all` drains the fleet to durable snapshots; after a daemon restart apps are re-adopted asleep and wake in place on demand. `scripts/smoke_upgrade.sh` rehearses it against the previous release — a stateless and a volume app sleep under the old daemon and wake **warm** under the new one, data intact — so cross-version snapshot-wake is measured, not assumed. A volume app that can't wake warm falls back to a cold create automatically.
- [x] **daemon backup:** `crucible admin backup` streams a tar.gz of the app store, tokens, volume records, and registry credentials (hot, via bbolt read transactions) plus a manifest; restore is a documented procedure onto a stopped daemon, after which the reconciler re-creates every app. Gated by a default-deny `admin_backup` scoped op. Volume *data* stays with `volume backup`.
- [x] **Disk visibility:** `snapshot_disk_bytes` / `volume_disk_bytes` / `backup_disk_bytes` on `/metrics` (sparse-aware), a Grafana disk panel, and a verified latest-per-instance snapshot-retention contract, so density is visible as disk, not just RAM.
- [x] **IPv6 at the edge:** the proxy and published ports accept IPv6 on a wildcard bind and do the family hop to the v4 guest; a published port can pin a v6 address (`-p '[::1]:8080:80'`). Guests remain IPv4-only.

### v0.6.3: Volume backups

A volume has a point-in-time backup you can restore into a new volume, plus a clone, so stateful data survives more than a running host: an accidental `rm`, a bad migration, application-level corruption, and (off-host) disk death ([backups.md](backups.md)).

- [x] **`volume backup` / `restore` / `clone`:** a consistency-aware, point-in-time copy of a volume, restorable into a new volume (never overwrites) or cloned straight into a new one. Copies reflink (O(1)) when the backup dir shares the volume filesystem, a full byte copy otherwise.
- [x] **Consistency by state:** a detached volume is copied directly; a slept app's volume is copied from its already-fsync'd backing file; a **live** volume is `FIFREEZE`d (only the volume mount, never the guest root), copied, then thawed, with an agent-side watchdog that auto-thaws if the daemon fails to.
- [x] **`--backup-dir`:** default `<volume-dir>/backups`; point it at another disk or a mounted store to keep backups off-host. A live backup needs a reflink-capable backup filesystem (btrfs/XFS); on ext4 it is refused (sleep the app instead).
- [x] **Full surface:** REST `/volumes/{name}/backups` + `/restore` + `/clone` and `/backups`, SDK methods, MCP `volume_backup` / `volume_restore` (**32 tools**), and new guest agent `/freeze` + `/thaw` ops.

## Planned

### Next: Production images & deploys

The app model, its front door, zero-downtime updates, operate-by-name, private-registry pull, scale-to-zero (HTTP and TCP), app→app networking, horizontal scale-out, observability, persistent volumes, wake-on-TCP serverless databases, instant (~170 ms) snapshot-wake for stateful apps, volume backups, off-host backups, upgrade-without-drop, daemon backup, and TLS termination + custom domains exist (v0.4.0–v0.7.0); next is usage metering, incremental backups, and the rest of production-grade deploys.

- • **Wildcard / DNS-01 certificates**: a single cert for `*.<domain>` via a DNS-01 solver, for apps whose per-name HTTP-01 challenge can't resolve to the host.
- • **Native cloud-registry auth**: ECR `GetAuthorizationToken` / GCP / Azure token exchange (and instance-identity creds), so cloud registries "just work" without re-feeding a short-lived token.
- • **PTY / full terminal.** The interactive shell is line-buffered today; a real PTY adds full-screen programs, colors, and Ctrl-C job control.
- • **Pause / freeze-for-forensics.** `crucible pause <id>` freezes a suspicious workload and snapshots it for analysis before you kill it, Firecracker pause + snapshot already exist under the hood; this surfaces them as a security-ops action.
- • **Growable live disk + accounting.** `--disk` sizes the writable rootfs at create today; this adds growing a live sandbox's disk and per-sandbox disk accounting.

### v0.4.x: Hardening & ecosystem

- • **More language profiles**: Rust, Java, Ruby, Swift, C/C++, bash-only, minimal-alpine.
- • **`policy.yaml`.** A single versionable artifact that supersets scoped tokens, quotas, syscall rules, network allowlists, and mount policies.
- • **Per-language seccomp policies.** Hand-tuned syscall allowlists per runtime; generic policies are too loose.
- • **DNS-layer allowlist filtering** and **packet capture on demand** (`crucible sandbox tcpdump …` → a pcap of everything the sandbox did on the network).

### v0.5.x: Observability

First-class, exportable telemetry (v0.5.0–v0.5.1 ship only the wake-latency / sleep-count / internal-request slices):

- • **Per-app request metrics.** RPS, latency percentiles, and error rates per app on `/metrics`, plus reference Grafana dashboards as code.
- • **OpenTelemetry export.** Every lifecycle event, exec, snapshot, and sleep/wake as OTel spans with stable semantic conventions; OTLP to Jaeger / Tempo / Datadog / Honeycomb / Grafana.
- • **Metering.** vCPU-seconds and RAM-GiB-hours that count slept time at ~0, the economic proof that scale-to-zero actually saves.
- • **Syscall tracing.** Optional per-sandbox syscall log via ptrace or eBPF, expensive to enable, valuable when you need to know exactly what an agent's code did.
- • **Filesystem diff.** `crucible fs diff sbx_…` shows every file created, modified, or deleted vs. the starting rootfs.
- • **Record and replay.** Capture a full execution trace (stdin/stdout, env, filesystem writes, network bytes) and replay it deterministically in a new sandbox.

### v0.6: Fork trees

Make parallel agent exploration a first-class workflow, not just a primitive. Fork lineage (v0.2.0) and the TUI tree view are the groundwork.

- • **Fork tree API.** Explicit parent/child relationships between snapshots, with depth limits and branch pruning.
- • **Scoring hooks.** Attach a scoring function to a fork tree; crucible prunes the lowest-scoring branches and keeps exploring the best. Beam search for code.
- • **Shared memory reads.** Children forked from the same snapshot read shared pages without duplication, cutting memory cost per fork.

### Longer term

Directions that matter once the core is solid. Not committed to a version or order yet.

- • **Persistent workspaces & richer interactive sessions.** Bidirectional stdin ships (`crucible shell` / `exec -i`) and a full PTY is planned (see Next); what's left is longer-lived named workspaces an agent reattaches to, and first-class REPL / language-server ergonomics on top of the shell.
- • **First-party agent integrations.** Native hooks and ready-made examples for Claude Code, Cursor, and common agent frameworks, building on the MCP server, plus a typed SDK (Python/TS) so the fork/snapshot workflow isn't hand-rolled over HTTP.
- • **Snapshot sharing.** A registry for warm setup-snapshots, boot a "Django project, dependencies installed" snapshot and run against it instantly.
- • **Published regression benchmarks.** The harness ships (`make bench`, [benchmarks.md](benchmarks.md)); tracking cold-start / fork-latency / throughput numbers over releases in CI is the remainder.
- • **Stable API + external security audit.** A versioned API with a deprecation policy and a published third-party audit, the bar for `v1.0`.
- • **WASM profiles.** WebAssembly sandboxes alongside VM sandboxes, for workloads where full-VM isolation is overkill.
