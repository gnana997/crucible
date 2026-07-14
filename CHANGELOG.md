# Changelog

All notable changes to crucible are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and crucible aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it
reaches `v1.0` — until then, `0.x` releases may change behavior as the design
settles.

## [0.6.6] — 2026-07-15

Off-host backups. A backup under `--backup-dir` still dies with the box; this
release lets a backup leave the host without putting any cloud SDK or credential
in the daemon. Two thin, provider-agnostic verbs stream a backup's bytes out and
back, so a control plane (or a cron script) ships them to wherever it keeps
backups.

### Added

- **`volume backup export <id>`** streams a backup's bytes off the host
  (`GET /backups/{id}/export`). Gzip by default — the ext4 image is sparse, so
  the holes compress away over the wire; `--raw` streams the backing file
  uncompressed. Write to a file (`-w`) or pipe it (`… | aws s3 cp - s3://…`).
- **`volume backup import --source <vol>`** streams a backup back onto a
  (possibly fresh) host and registers it (`POST /backups/import`), printing a new
  backup id; `volume restore --from <id> --to <new>` then materializes a volume.
  The imported backup takes this host's id — it lives here now.
- **New default-deny `volume_backup` scoped-token op** gates export and import:
  they move volume data across the API boundary, so a `read`-only token can't.
  Creating a backup on-box stays `snapshot`-grade. SDK `ExportBackup` /
  `ImportBackup`. No MCP tools (an agent must not stream volume data off the box).

### Fixed

- **Port-publish forwarder shutdown race.** `Set.Close` could call
  `wg.Wait` while an accept loop was still spawning per-connection handlers with
  `wg.Add(1)` — a data race on the WaitGroup (caught by `go test -race`). The
  accept loop is now counted in the WaitGroup, so the counter can't be observed
  at zero while it is live. Pre-existing (v0.4.1); no behavior change.

### Notes

- The daemon stays provider-agnostic on purpose: no S3/GCS SDKs, no cloud
  credentials. A self-hoster's simplest off-host path is still `--backup-dir` on
  an NFS/S3-fuse mount; export/import are what a **remote** control plane uses to
  pull backups over the network and ship them onward (where it can also add
  incremental / deduplicated storage). Docs: [backups.md](docs/backups.md);
  acceptance: `scripts/smoke_offhost_backup.sh` (full export → import → restore
  round trip + the `volume_backup` gate).

## [0.6.5] — 2026-07-15

Capacity guards for scale-to-zero at scale. Density is the whole economic bet —
pack more sleeping apps than host RAM — which makes "everything wakes at once" a
designed-for scenario. This release proves that herd is graceful and adds the
missing disk half of the admission story.

### Added

- **Sleep disk-admission floor.** A sleep (snapshot) writes a full
  guest-RAM-sized memory file under `--work-base`, so a fleet snapshotting to a
  nearly-full disk could otherwise fill it. `--sleep-min-free-disk-mib` (default
  1024) refuses a sleep when free disk is below the floor; the app stays running
  (RAM-backed, the safe degraded state, and the signal to add disk). This is the
  disk complement to the existing `--wake-min-free-mib` RAM floor; both are
  fail-open (a `statfs` / `/proc` read error admits).
- **Mass-wake load test.** `scripts/bench_masswake.sh` boots N scale-to-zero
  apps, drains them with `app sleep --all`, then fires N **concurrent** wakes and
  reports the wake-latency distribution (p50/p90/p99/max, client and daemon),
  how many wakes the RAM floor deferred to a clean `503` + retry, and the host
  MemAvailable low-water mark. Measured on a 16 GiB-free box: 20 concurrent wakes
  served with a p99 of ~430 ms (daemon) / ~520 ms (client) and **no meaningful
  RAM movement** — lazy paging faults in only each guest's working set, not its
  full allocation, so the herd is safe by construction ([apps.md](docs/apps.md),
  [benchmarks.md](docs/benchmarks.md)).

## [0.6.4] — 2026-07-15

Operate with confidence. Upgrade the daemon without losing your apps: drain the
fleet to sleep, swap the binary, and every app is re-adopted and wakes on demand
— rehearsed against the previous release so cross-version snapshot-wake is
measured, not hoped for. Plus a one-command daemon backup, disk-usage
visibility for scale-to-zero density, and IPv6 at the edge.

### Added

- **Upgrade-without-drop.** `crucible app sleep --all` drains every running app
  to a durable snapshot; after a daemon restart the apps are re-adopted asleep
  and wake in place on the next request. The runbook is
  [docs/upgrades.md](docs/upgrades.md), and `scripts/smoke_upgrade.sh` rehearses
  it end-to-end against the previous release tag — a stateless app and a
  volume-backed app sleep under the old daemon and wake warm under the new one,
  data intact. Warm cross-version wake is the measured result; a volume app that
  can't wake warm falls back automatically to a cold create from its image.
- **`crucible admin backup`.** Streams a tar.gz of the daemon's persistent
  state — the app store, token file, volume records, and registry credentials,
  plus a manifest — taken hot (bbolt read transactions) while the daemon keeps
  serving. Volume *data* is not included; pair it with `volume backup`. Restore
  is a documented procedure onto a stopped daemon
  ([docs/backups.md](docs/backups.md)); on restart the reconciler re-creates
  every app from the restored records. Gated by a new **default-deny
  `admin_backup`** scoped-token op (the archive carries usable registry
  secrets). SDK `AdminBackup`.
- **Disk-usage metrics.** `snapshot_disk_bytes`, `volume_disk_bytes`, and
  `backup_disk_bytes` on `/metrics` (sparse-aware — allocated blocks, not
  logical size), so scale-to-zero density is visible as disk, not just RAM.
  Reference Grafana dashboard gains a disk panel. A slept app keeps exactly one
  snapshot set (retention is verified in `scripts/smoke_leaks.sh`).
- **IPv6 at the edge.** The ingress proxy and published ports accept IPv6 on a
  wildcard bind (`--proxy-listen :80`, `-p 8080:80`) — the proxy does the family
  hop to the v4 guest. A published port can be pinned to a v6 address with
  docker's bracket syntax (`-p '[::1]:8080:80'`). Guests remain IPv4-only
  ([docs/network.md](docs/network.md)).

### Fixed

- **No dropped connection when a request races `app sleep`.** A connection that
  arrived while an app was mid-snapshot could be reset instead of held: the
  instance was marked non-routable only *after* its VM stopped, so the ingress
  resolver's brief cache window still routed to the paused guest. The sleep
  transition now marks the instance non-routable before it pauses, so a racing
  request resolves as asleep and is queued behind the wake.

### Security

- **App→app authorization is bound to the caller's source IP, verified.**
  `scripts/smoke_leaks.sh` now asserts that an un-granted app is refused a peer
  another app may reach (the grant follows the source IP, so a recycled guest
  `/30` cannot inherit a deleted app's reach), and that a proxied response
  exposes no internal guest IP to external clients.

## [0.6.3] — 2026-07-14

Volume backups. A persistent volume now has a point-in-time backup you can restore
into a new volume, plus a clone. Backups are consistency-aware: a detached or slept
volume is copied directly (quiescent), and a **running** database is frozen with
`fsfreeze` for the instant of the copy, so it is backed up with no downtime. The
copy reflinks (O(1)) when the backup directory shares the volume filesystem, so it
is cheap; point `--backup-dir` at another disk or mount to keep backups off-host.

### Added

- **`volume backup`** takes a filesystem-consistent, point-in-time copy of a
  volume, restorable to a new volume. `volume backup ls [<name>]` lists backups and
  `volume backup rm <id>` deletes them. Backups are recorded durably (a second
  bbolt bucket) and copied via reflink when the backup filesystem supports it, a
  full byte copy otherwise.
- **Consistency by state.** A detached volume is copied directly; a slept
  scale-to-zero app's volume is copied from its already-fsync'd backing file; a
  live volume is `FIFREEZE`d (only the volume mount, never the guest root),
  copied, then `FITHAW`ed, with an agent-side watchdog that auto-thaws if the
  daemon fails to, so a live backup can never leave a guest frozen.
- **`volume restore --from <id> --to <name>`** materializes a backup into a new
  volume (never overwrites an existing one), and **`volume clone <src> <dst>`**
  copies a volume straight into a new, independent one (fork a database for a
  preview or test environment).
- **`--backup-dir`** daemon flag (default `<volume-dir>/backups`). A same-filesystem
  backup dir reflinks; another disk or mount survives disk death but copies in full.
- REST `POST /volumes/{name}/backups`, `GET /backups`, `POST
  /volumes/{name}/restore`, `POST /volumes/{name}/clone`; SDK `BackupVolume` /
  `ListBackups` / `DeleteBackup` / `RestoreBackup` / `CloneVolume`; MCP tools
  `volume_backup` and `volume_restore` (**32 MCP tools** total). New guest agent
  `POST /freeze` and `POST /thaw` ops. Docs: [backups.md](docs/backups.md).
  Acceptance: `scripts/smoke_backups.sh`.

### Notes

- A **live** backup requires a reflink-capable backup filesystem (btrfs or XFS):
  freezing a guest for a full byte copy would be too disruptive, so a live backup
  on ext4 is refused (sleep the app, or back up a detached or slept volume, which
  works on any filesystem). Off-host targets (S3, rsync) and incremental backups
  are planned for a later release.

## [0.6.2] — 2026-07-14

Instant serverless-stateful. A volume-backed app (the serverless postgres/redis
from v0.6.1) used to cold-boot on wake: sleep destroyed the instance and wake
booted a fresh one, so the database ran recovery (seconds). Now it snapshot-sleeps
and snapshot-wakes like a stateless app: the process is already running in the
restored memory, attached to its volume, so wake takes about 170 ms (reflink;
~240 ms on ext4) with no cold boot and no WAL recovery. The cold-start wart on
serverless postgres is gone.

### Changed

- **Volume apps snapshot-sleep and wake in place.** A scale-to-zero app on a
  volume now sleeps by snapshotting its instance and stopping the VMM (RAM freed,
  the single-writer volume guard held) instead of destroying it, and wakes by
  restoring the snapshot with the volume re-attached: same instance and IP,
  ~170 ms (reflink), data intact. A wake after a **daemon restart** restores a fresh
  instance from the durable snapshot (new IP, still no cold boot), re-acquiring
  the volume guard.
- The published-port latency table in [serverless.md](docs/serverless.md) now
  reads ~170 ms for volume-backed wake (was cold boot); see
  [benchmarks.md](docs/benchmarks.md) for the measured distribution.

### Added

- **Durable-while-asleep fsync.** The volume backing file is fsync'd host-side
  before the VMM stops at sleep (Firecracker does not flush drive backing files on
  snapshot), so a host crash while a volume app is asleep cannot lose committed
  rows — the v0.6.0 fsync-honest guarantee holds across sleep.
- **Automatic cold-boot fallback.** If a snapshot restore ever fails, the wake
  falls back to a stop/start cold-create (tearing down the slept instance and
  re-attaching the volume) so a wake never fails — it just isn't instant that once.

## [0.6.1] — 2026-07-13

Wake-on-TCP: scale-to-zero for **any** self-hosted TCP service, not just HTTP.
Scale-to-zero used to work only for HTTP apps woken through the ingress proxy; a
database, cache, or broker is reached over a published port, so it couldn't sleep.
Now a scale-to-zero app's published port is fronted by an L4 forwarder that wakes
the app on the first TCP connection and sleeps it when it goes idle, with no proxy
in the path, protocol-agnostic. The result: a **self-hosted serverless postgres or
redis** (cold-starts on the next connection, data intact on its volume), and a
**scale-to-zero pub/sub** backend that sleeps when nobody's connected.

### Added

- **Wake-on-TCP.** A scale-to-zero app (`--min-scale 0 --idle-timeout <dur>`) that
  publishes a host port (`-p HOST:GUEST`) gets an **app-scoped waking forwarder**:
  it owns the host port for the app's whole life (surviving its sleep), resolves
  the current instance fresh per connection, **wakes a slept app on connect**
  (a connection burst coalesces into one wake), and forwards. TCP connection
  activity feeds the idle monitor, so the app sleeps on TCP inactivity: all
  automatic, no new daemon flag, no ingress proxy required. Works for postgres,
  mysql, redis, mongo, or your own daemon. Docs: [serverless.md](docs/serverless.md).
  Acceptance: `scripts/smoke_serverless.sh`.
- **Idle-connection reaping** (`--connection-idle-timeout`, default = `--idle-timeout`).
  A pooled client that holds a connection open between queries would keep the app
  awake forever; the forwarder closes a byte-idle connection after this timeout so
  the app can reach zero connections and sleep. The client's pool reconnects on its
  next query. This makes serverless work for connection-pooled databases/caches, not
  just connect-per-request clients.
- **Connection-scoped mode** (`--keep-connections`). Turns reaping off: the forwarder
  never closes a connection on silence (only TCP keepalive reaps a dead peer), so the
  app stays awake while any client is connected and sleeps only when the **last**
  disconnects. This is the mode for pub/sub, `LISTEN/NOTIFY`, and streaming, where an
  idle-but-live subscription must not be dropped.
- **Serverless postgres & redis.** A volume-backed postgres with `--min-scale 0`
  sleeps (stop/start, freeing the VM) and cold-wakes on the next connection with its
  data intact; a redis scales to zero between requests (reaping) or stays awake for a
  live subscriber (`--keep-connections`).

### Changed

- A scale-to-zero app is now valid with **either** an HTTP `--port` (proxy wake)
  **or** a published host port (TCP wake); an app with neither wake trigger is
  rejected at create time (it could never come back), instead of silently running
  always-on.

### Fixed

- **Guest `/dev/fd`.** The guest init now creates the standard `/dev/fd` and
  `/dev/std{in,out,err}` → `/proc/self/fd` symlinks a bare devtmpfs lacks. Without
  them, bash **process substitution** `<(…)` fails, which broke container
  entrypoints that use it, e.g. the postgres image's password init
  (`initdb --pwfile=<(…)`). Any image relying on `<(…)` or `/dev/stdout`-by-name
  now works.

## [0.6.0] — 2026-07-13

Persistent volumes. Data that outlives the sandbox: a named, fsync-honest block
device you attach to a database's data directory, a browser profile, or an
upload folder — surviving destroy/re-create, a hard VM kill, an app redeploy,
sleep, and a daemon restart. Stateless workloads keep the snapshot/fork magic;
stateful ones trade it for single-writer correctness.

### Added

- **Volumes on sandboxes.** `crucible run --volume NAME:/path` (and
  `sandbox create --volume`) attaches a durable block device, created and
  formatted ext4 on first use and reattached by name thereafter. Backed by a
  sparse file under the daemon's new `--volume-dir`, attached as a second
  Firecracker drive with `cache_type=Writeback` so a guest `fsync` reaches the
  host — committed data survives a hard kill of the VM.
- **Volume lifecycle.** `crucible volume create <name> [--size 5G]` / `ls` / `rm`
  (rm refused while attached); a durable bbolt record store that survives daemon
  restarts; REST `/volumes` (`POST`/`GET`/`DELETE`); and MCP tools
  `volume_create` / `list_volumes` / `delete_volume` (**30 MCP tools** total).
- **Volume-backed apps.** `crucible app create --volume NAME:/path` gives a
  durable app a volume. Being single-writer, a volume app redeploys via
  **destroy-then-boot** (not the zero-downtime flip), sleeps via **stop/start**
  (quiesce → destroy → cold-create on wake, not a snapshot), and cannot scale
  out. Its data survives `app update`, sleep, and daemon restarts.
- **Daemon flags** `--volume-dir` (enables volumes; must share a filesystem with
  `--chroot-base`) and `--volume-default-size` (default 2 GiB).
- Docs: [volumes.md](docs/volumes.md). Acceptance: `scripts/smoke_volumes.sh`.

### Notes

- A volume app without `--port` cannot idle-sleep yet (wake-on-connection over
  TCP is a later release); it runs always-on. Volume-app redeploy and wake are
  **not** zero-downtime — an inherent cost of single-writer storage.

## [0.5.4] — 2026-07-13

Observability. A running app is now legible — per-app metrics, OTLP export of
metrics and logs, daemon profiling, and on-demand packet capture — with a
deliberately small in-daemon surface: crucible emits open standards and delegates
routing to your collector.

### Added

- **Per-app metrics on `/metrics`.** Request rate, latency (histogram), and status
  class per app from the ingress proxy (`app_requests_total{app,code}`,
  `app_request_duration_seconds{app}`), plus lifecycle gauges pulled from the app
  manager — `app_replicas`, `app_ready_replicas`, `app_up`, `app_asleep`,
  `app_sleep_total`, `app_last_wake_latency_ms`. Label cardinality is bounded to
  real apps (an unknown Host header is never counted). A **reference Grafana
  dashboard** ships in `docs/observability/grafana-dashboard.json`.
- **OTLP export (metrics + logs).** One flag — `--otlp-endpoint` — pushes the same
  `/metrics` series over OTLP (via an OpenTelemetry Prometheus **bridge**, so no
  metric is redefined and `/metrics` is unchanged) and streams app logs from the
  durable log store as OTLP log records. Honors the standard `OTEL_EXPORTER_OTLP_*`
  / `OTEL_RESOURCE_ATTRIBUTES` env natively; `--otlp-protocol grpc|http`,
  `--otlp-headers`, `--otlp-insecure`, `--otlp-logs`. Point it at any collector
  (Grafana/Tempo/Loki, SigNoz, Datadog, Honeycomb, …). Off by default; a setup
  error is logged and skipped, so the daemon always starts and `/metrics` keeps
  serving.
- **Daemon profiling.** `--pprof-listen 127.0.0.1:6060` serves Go `net/http/pprof`
  for CPU/heap/goroutine profiles of the daemon's hot paths. Off by default; warns
  on a non-loopback bind (profiles can expose process memory).
- **On-demand packet capture.** `crucible sandbox capture <id>` / `app capture
  <name>` streams a live **pcap** of a guest's traffic, captured **host-side** on
  the sandbox's veth — **no in-guest tcpdump**, so it works for distroless/scratch.
  BPF `--filter`, `--snaplen`, and hard `--max-bytes` / `--max-seconds` caps.
  Gated by a new **default-deny `capture` scoped-token op** (never implied by
  `read` — payloads are sensitive) and audited. Needs `tcpdump` on the host.
- **MCP tools (24 → 27).** `list_images` and `delete_image` (image-cache
  management), and `capture` (writes a pcap to a local file and returns its path,
  gated by the `capture` op).

## [0.5.3] — 2026-07-13

Reliability & isolation hardening: no orphaned VMs across app-lifecycle edges, no
stale guest agent after a daemon upgrade, and app→app networking no longer blocks
publishing an app on the same host port.

### Fixed

- **No orphaned instances across app-lifecycle edges.** A rolling `app update`
  keeps the old instance alive briefly to drain. Deleting the app, issuing a
  second update, or sleeping it *during* that window used to leave that VM
  running forever (the drain slot was reaped only while the app stayed desired).
  Teardown now destroys **every** instance an app owns (current + draining +
  incoming); a superseding update destroys the prior draining instance before
  reusing the single drain slot; and sleep frees any in-flight roll instances —
  so an asleep app truly runs zero VMs. Covered by `scripts/smoke_leaks.sh`.
- **Upgrades no longer boot a stale guest agent from a cached image.** Converted
  OCI images are now keyed by the injected agent's digest as well as the source
  image digest: a daemon whose embedded agent changed (an upgrade) re-converts on
  next use instead of silently reusing an old conversion. This is what made
  scale-to-zero *wake* fail on an image an older daemon had already cached — the
  manual `crucible image rm` workaround is no longer needed.
- **A published host port coexists with app→app networking on the same port.**
  With `--internal-networking`, the `<app>.internal` VIP binds its port on a
  host-local address; publishing an app to that same host port (`-p 80:80` / `-P`
  an EXPOSE-80 image while the VIP is on `:80`) used to fail with `address already
  in use`. Both listeners now set `SO_REUSEPORT`, and a host-port registry
  preserves one-owner-per-port, so the wildcard publish and the specific VIP
  coexist (Linux routes each connection to the most-specific bind).

## [0.5.2] — 2026-07-13

Scale out. An app runs multiple replicas behind the ingress proxy, load-balanced,
and autoscales on request concurrency — each replica forked **warm** from a
snapshot in milliseconds, not cold-booted.

### Added

- **Horizontal scaling.** `app create <app> --min-scale N` runs N warm replicas
  behind the proxy. Each is stamped by **forking a golden snapshot** of the
  healthy primary (lazy memory; clone-safe — a distinct machine-id and IP per
  replica), so scaling up is cheap. The reconciler self-heals the fleet: a
  replica that dies is replaced.
- **Load balancing.** The proxy balances requests across an app's live instances
  with **power-of-two-choices least-request** selection, a slow-start ramp so a
  just-forked replica isn't slammed while its cache is cold, and passive outlier
  ejection (a repeatedly-failing instance is dropped and re-forked). External and
  app→app (`<app>.internal`) traffic both balance through the one path.
- **Autoscaling.** `--max-scale M` (with `--target-concurrency C`) autoscales an
  app between its floor and M on observed request concurrency: a fast window
  scales **up** on bursts, a slow window scales **down** when calm (after a
  stabilization window, so it doesn't flap). `min_scale=0` composes with
  scale-to-zero — idle sleeps to 0, a request wakes to 1, and load scales 1→M.
- **Surfacing.** `app ls` gains a `REPLICAS` column (ready/desired); `app get`
  reports the full instance set (`replicas`, `ready_replicas`, `instances`).

Horizontal scale-out is for **stateless** apps (a shared database still waits for
volumes). A multi-instance app must be proxy-fronted (a `--port`, no fixed host
publish — two instances can't co-bind a host port).

## [0.5.1] — 2026-07-12

App→app service networking. Deploy your frontend and API as separate apps; the
frontend reaches the API by name — `backend.internal` — over the ingress proxy,
default-deny, and a scaled-to-zero callee wakes on the internal call.

### Added

- **Reach another app by name.** With the daemon's `--internal-networking`, an
  app calls another at `http://<app>.internal/`, routed through the ingress proxy
  to the callee's current instance. Traffic goes through the proxy VIP (the DNS
  anycast), not a direct guest-to-guest path — so an internal call inherits
  **wake-on-request** (a scaled-to-zero callee wakes and serves) and leaves
  per-sandbox network isolation intact (a guest still can't reach a peer's IP
  directly).
- **Default-deny authorization.** `crucible app create <app> --can-call <other>`
  (repeatable) declares the apps an app may call; empty means it may call none.
  Enforced daemon-side at two layers: the ingress proxy returns **403** on an
  un-granted call, and the DNS answers `<app>.internal` only for granted callers
  (otherwise **NXDOMAIN** — a guest can't even *discover* an app it may not
  call). On the Go SDK (`AppSpec.CanCall`) and MCP (`create_app`/`update_app`
  `can_call`).
- **Metric.** `app_internal_requests_total` on `/metrics` counts authorized
  app→app requests.

App→app networking is experimental and **off by default** (`--internal-networking`;
`--internal-proxy-port`, default 80). This is the stateless frontend→API tier;
stateful apps (a shared database) still wait for volumes.

## [0.5.0] — 2026-07-12

Scale to zero. An app sleeps when idle and wakes on the next request in under a
second — same IP, same identity, correct clock — and survives a daemon restart
while asleep. Built by re-pointing machinery crucible already ships
(snapshot/restore with lazy memory, clone-safety, the reconciler, the ingress
proxy) at a new policy, not by inventing a new subsystem.

### Added

- **App sleep/wake.** `crucible app sleep <name>` snapshots a running app and
  stops its VMM to free RAM + CPU while keeping the netns, subnet/IP reservation,
  and ingress route; `crucible app wake <name>` restores it **in place** — same
  instance id, same IP, no DHCP bounce — reseeding the guest CRNG and stepping
  its clock to host time *before the app is reachable*, but (unlike a fork)
  **not** rotating machine-id/hostname. On the Go SDK (`SleepApp`/`WakeApp`,
  `App.Sleep`/`App.Wake`) and MCP (`app_sleep`/`app_wake`). Status now reports
  the `asleep` and `waking` phases, `last_wake_latency_ms`, and `sleep_count`.
- **Automatic scale-to-zero.** `crucible app create --idle-timeout <dur>` (with
  `--min-scale 0`) drops an app to ~zero RAM once it has been idle: the ingress
  proxy tracks per-app last-activity + open connections, and once the app is idle
  and healthy the reconciler sleeps it. The next request through the proxy
  **triggers a wake, holds the request, and forwards it when the app passes its
  readiness probe** — a herd of requests hitting one sleeping app coalesces into
  a single wake. `--min-scale ≥1` keeps ≥1 instance always-warm (today's
  behavior); `--idle-timeout 0` never sleeps.
- **Survives a daemon restart while asleep.** Sleep captures a **durable**
  snapshot (journaled record + cloned rootfs), so a slept app is re-adopted on
  daemon start and the first post-restart request wakes a fresh instance from it.
- **Wake admission gate.** A wake is refused (the request gets a `503` and the
  app stays asleep) when host free memory is below a floor
  (`--wake-min-free-mib`, default 256) rather than thrashing the box. The live
  snapshot count is exported as `snapshots_active` on `/metrics`, alongside an
  `app_wake_latency_seconds` histogram.

### Changed

- The MCP server gains `app_sleep` / `app_wake` — **22 → 24 tools**.

## [0.4.4] — 2026-07-12

Private registries. Pull authenticated images — so `run`, `app create`, and an
app's re-pull on restart can fetch private images, with the credential living on
the daemon (not your local docker config).

### Added

- **Pull from private/authenticated registries.** `crucible registry login
  <host>` stores a per-registry `(username, secret)` credential on the daemon,
  which feeds every image pull — `run`, `sandbox create --image`, `app create`,
  and (critically) an app's **re-pull on restart or reboot**, so a durable app on
  a private image survives a daemon restart. `registry ls` lists host + username
  (**never the secret**); `registry logout` removes one. Log in with
  `--password-stdin`, `--password`, or a masked prompt. Static credentials cover
  Docker Hub, GHCR, GitLab, Quay, self-hosted registries (Harbor/Nexus/
  Artifactory), and the static forms of GCP (`_json_key`) and Azure ACR; AWS ECR
  works with an `aws ecr get-login-password` token (re-run every ~12h). See
  [docs/registry.md](docs/registry.md).
- **One-shot per-request credentials.** `crucible run` / `sandbox create` take
  `--registry-auth USER:SECRET` (or the `CRUCIBLE_REGISTRY_AUTH` env var) for a
  CI/throwaway pull — used for that pull only, never stored, and taking
  precedence over a stored credential. Also `registry_auth` on the create /
  `POST /images` request bodies (Go SDK: `Client.RegistryLogin`/`RegistryLogout`/
  `ListRegistryCredentials`).
- **Scoped-token `registry` operation.** Managing credentials
  (`POST`/`DELETE /registry/credentials`) is gated by a new `registry` policy
  operation; listing needs only `read`. Credentials are stored `0600` (usable,
  **not encrypted at rest** — they must be replayed to the registry) and are
  never read from `~/.docker/config.json`, so a host login can't leak into the
  root daemon.

## [0.4.3] — 2026-07-12

Operate & safe-update. Updating a deployed app no longer drops traffic, and you
can drive a running app **by name** — exec, logs, shell — from the CLI, SDK, and
MCP, with resolution that survives a self-heal or redeploy.

### Added

- **Zero-downtime rolling `app update`.** For a proxy-fronted app (a `--port`, no
  fixed host publish) an update no longer destroys-then-boots. The reconciler
  boots the new instance, waits for it to pass a **readiness gate** (its health
  check, or — with none — a TCP connect to the app's port), **flips the ingress
  route** to it, then drains the old instance for a few seconds before destroying
  it. The proxy follows the flip automatically, so the cutover drops nothing. A
  failed update (the new instance never becomes ready within the rollout
  deadline, or crash-loops) **aborts and keeps the old instance serving**,
  recording the failure — it never takes the app down. Other apps keep the
  destroy-then-boot path. New `status.instance_generation` shows which spec the
  live instance is serving (it lags `generation` during a roll or a failed
  update). See [docs/apps.md](docs/apps.md).
- **Operate an app by name — daemon routes.** `POST /apps/{name}/exec` (one-shot
  or `?stdin=1` interactive), `GET /apps/{name}/exec` (WebSocket interactive), and
  `GET /apps/{name}/logs` resolve the app to its **current** instance server-side
  **per request**, then delegate to the sandbox exec/logs handlers — so a call
  issued across a self-heal or rolling update always targets the live instance.
- **Redeploy-safe CLI/SDK.** `crucible app exec` / `app shell` / `app logs` (and
  the SDK `App.Exec`/`App.Logs`, `Client.AppExec`/`AppExecInteractive`/`AppLogs`)
  now use the name-based routes instead of resolving the instance once on the
  client. `app logs -f` **reattaches** to the new instance across a redeploy
  (with a `== reattached to <id> ==` marker). `app exec` gained
  `--cwd`/`--timeout`/`-e,--env` and `app shell` gained `--shell`, matching the
  sandbox commands.
- **MCP `app_exec` / `app_logs`.** Operate a deployed app by name from an agent —
  resolved to the current instance per call (→ 22 tools). `app_exec` is
  exec-gated, `app_logs` read-gated.

## [0.4.2] — 2026-07-12

Reach it by name. The durable app from v0.4.0/v0.4.1 is now reachable through a
daemon-owned ingress proxy — many apps on one host, addressed by name, the route
following the app across self-heal and redeploy — plus in-place `app update` and
image-`HEALTHCHECK` seeding.

### Added

- **Reach an app by name — the ingress proxy.** A daemon-owned front door routes
  inbound traffic to an app's *current* instance by name, so many apps share one
  host and the route follows the app across self-heal/redeploy. Off by default;
  enable with `--proxy-listen :80` (Host-header routing, L7), `--proxy-tls-listen
  :443` (SNI passthrough, L4 — the guest terminates its own TLS), and
  `--proxy-domain <domain>` (`web.<domain>` → app `web`). New `--port` on an app
  picks the guest port to forward to (defaults from a single published/`EXPOSE`d
  port). In-process, resolution is live so it never routes a stale IP; unknown
  host → 404, no ready instance → 502. See [docs/proxy.md](docs/proxy.md).

- **Update a running app — `crucible app update <name>` / `PUT /apps/{name}`.**
  Replaces the app's spec (same flags as `create`; name immutable) and redeploys
  its instance — the daemon bumps the app's generation and the reconciler
  destroys the old instance and boots a fresh one from the new spec. Desired
  running/stopped is retained. Also on the Go SDK (`UpdateApp`) and the MCP
  `update_app` tool (→ 20 tools).
- **Seed app health from the image's `HEALTHCHECK`.** An app that declares no
  health of its own now inherits the image's Docker `HEALTHCHECK` when present —
  derived as an `exec` check at first boot and persisted. `--health`/`--health-cmd`
  still override.

## [0.4.1] — 2026-07-12

Apps you can actually deploy: real config, real egress. Everything here builds on
v0.4.0 durable apps and applies across `app create`, `run`, and `sandbox create`.

### Added

- **Real egress for trusted workloads.** Two opt-in modes widen egress past
  the hostname allowlist, on `app create`, `run`, and `sandbox create`:
  `--net-full-egress` (reach any public host) and `--net-allow-cidr
  203.0.113.0/24` (reach IP literals in a public prefix). The invariant is
  **public unicast only** — metadata/link-local (`169.254.169.254`), RFC1918,
  CGNAT, loopback, and the reserved ranges are always dropped; a wholly-private
  CIDR reaches nothing. The nft drop list is unit-tested to agree with the DNS
  proxy's `IsPublicUnicast` guard so the two can't drift. Gated by a new
  `net_full_egress` scoped-token policy grant (default off, so a `net_allow_max`
  hostname ceiling can't be bypassed) and the MCP server's `--net-allow-max`
  guardrail. Also on the MCP `create_app`/`create_sandbox`/`run` tools.

- **Exec health checks — `crucible app create --health-cmd '<command>'`.** The
  daemon runs the command in the guest over vsock (exit 0 = healthy), joining the
  existing `http`/`tcp` probes; works even for an app with no network. Also on the
  MCP `create_app` tool (`health_type: "exec"` + `health_cmd`). (Auto-seeding a
  check from an image's own Docker `HEALTHCHECK` remains a follow-up.)

- **App env config — `crucible app create -e/--env KEY=VALUE` (repeatable).**
  Environment variables are delivered to the app's entrypoint (image `ENV` <
  your `--env`, so yours win). Surfaced on the CLI and the MCP `create_app` tool
  (`env`); the daemon already carried the field.
- **Publish an image's declared ports — `-P`/`--publish-all`.** On `app create`,
  `run`, and `sandbox create`: publishes every port the image `EXPOSE`s, each to
  the same host port number (guest N → host N — deterministic, unlike docker's
  random-host-port `-P`). tcp only; an explicit `-p` for a guest port wins. Also
  on the MCP `create_app` / `create_sandbox` tools (`publish_all`).

## [0.4.0] — 2026-07-11

### Added

- **Durable apps (v0.4) — workloads that survive a daemon restart and
  self-heal.** `crucible app create <name> --image … -p H:G --restart always
  --health http:PORT[:PATH]` promotes a workload to a named app the daemon keeps
  a healthy instance of: it restarts the instance on failure with exponential
  backoff and a crash-loop guard, health-checks it (http/tcp) and restarts on
  sustained failure, and — the headline — **re-creates it from persisted desired
  state after a daemon restart or host reboot** (a bbolt control-plane store +
  a reconcile loop; the ephemeral `sandbox` primitive is unchanged). Full
  surface: `app ls|get|rm|logs|exec|shell`, REST `/apps`, Go SDK (`CreateApp`/
  `ListApps`/`GetApp`/`DeleteApp` + an `App` handle), and four MCP tools
  (`create_app`/`list_apps`/`get_app`/`delete_app`), bringing the MCP surface to
  **19 tools**. Durability is desired-state reconcile (re-create), not live-VM
  re-attach — see the updated threat-model INV-6.

## [0.3.4] — 2026-07-11

### Added

- **`crucible fork -p HOST:GUEST`** — publish a host port on a fork (`docker
  run -p` semantics for copies). The fork API accepts an optional JSON body
  (`{"count", "publish"}`); publishing requires count 1 since host ports are
  exclusive. Forwarders roll back with the fork, close on delete, and appear
  in the fork's `published` field. Go SDK: `Fork(ctx, id, count, publish...)`
  (variadic, source-compatible).

## [0.3.3] — 2026-07-11

**The SDK foundation release.** The typed client and wire types are now a
public, dependency-free Go module (`github.com/gnana997/crucible/sdk`,
versioned independently as `sdk/vX.Y.Z`), the whole REST contract fans out
from one drift-guarded OpenAPI spec into generated TypeScript and Python
types, interactive exec gained a WebSocket transport any language can speak,
and the binary frame protocol is now specified and fixture-tested so SDKs in
new languages can be built — and verified — without a daemon or KVM.

### Added

- **Interactive exec over WebSocket** — `GET /sandboxes/{id}/exec` + upgrade:
  the cross-language transport for full-duplex exec (fetch-style HTTP stacks
  can't speak the hijacked `?stdin=1` stream; WebSocket also traverses L7
  proxies). First message is the JSON `ExecRequest`; after that the binary
  message payloads carry the exact same frame stream as the hijacked path.
  Gated as `exec` (not `read`) under scoped tokens. See [docs/wire.md](docs/wire.md).
- **Public Go SDK** — `internal/client` + `internal/api` + the shared wire
  types promoted to a nested, **zero-dependency** Go module: package
  `crucible` (client + `Sandbox`/`Snapshot` handles), `sdk/api` (REST DTOs),
  `sdk/wire` (frame codec + exec/service/files shapes). Typed errors
  (`ErrNotFound`/`ErrUnauthorized`/`ErrPolicyDenied` over structured
  `*Error`), pagination-ready `Page[T]` lists, SDK-owned `Identity`. The CLI,
  TUI, and MCP server now run on the same public package.
- **`docs/wire.md`** — the language-neutral wire spec: frame layout, chunking
  and termination rules, both interactive transports, files/tar semantics,
  and a four-step conformance recipe for SDK authors.
- **Conformance fixtures** (`sdks/fixtures/`) — recorded frame streams +
  manifest, generated from the real codec, so an SDK codec in any language is
  testable with no daemon and no KVM. Guarded by CI drift checks.
- **Generated TS + Python types** — `make gen` fans the OpenAPI spec out to
  `sdks/ts/src/schema.gen.ts` (openapi-typescript) and
  `sdks/python/crucible/models.py` (Pydantic v2 via datamodel-code-generator),
  pinned versions, CI `codegen-drift` job.
- **TypeScript SDK scaffold** (`sdks/ts`) — zero-runtime-dependency fetch
  client with streaming exec, typed errors, and a hand-written frame codec
  passing the full conformance suite; not yet published to npm.
- **`smoke_ws_exec.sh`** + `scripts/wsexec` — real-KVM smoke for the
  WebSocket transport, driving it exactly as a non-Go SDK would.

### Changed

- OpenAPI schema components renamed `Agentwire*` → `Wire*` (internal package
  name no longer leaks into the public spec).
- `internal/agentwire` now holds only the private daemon↔guest protocol
  (identity refresh, static network config, vsock port); the client-visible
  wire contract lives in the public `sdk/wire`.

## [0.3.2] — 2026-07-09

**The "drop your code in and run it" release.** Push local files straight into a
running sandbox with no image build and no Dockerfile, read bounded file content
back, watch a sandbox's logs live in the TUI, and stand the whole thing up on
Linux with a single command that provisions everything (including the guest
kernel) and starts the service. The launch line: it's the safe `docker run` for
untrusted or agent-written code, now with a first-class way to get code *in*.

### Added

- **`crucible cp <src> <id>:<dest>`** — push a local file or directory into a
  running sandbox as a tar stream over HTTP; the guest agent extracts it into
  the sandbox rootfs, rejecting path escapes (`..`, absolute paths, and symlinks
  that would leave the destination). No image build, no Dockerfile.
- **MCP `write_files` and `read_file`.** `write_files` drops content into a
  sandbox (exec-gated); `read_file` reads **bounded** file content back (UTF-8,
  base64 for binary, with a `truncated` flag), read-gated. crucible deliberately
  returns file content only and does not extract archives onto the host. Brings
  the MCP surface to **15 tools**. See [docs/mcp.md](docs/mcp.md).
- **TUI live logs view** (`l`): tail a sandbox's **durable** logs (service output
  + exec activity) live in a scrolling pane, each line timestamped with `stderr`
  highlighted, auto-following the tail unless you scroll up. The "docker + k9s"
  view; because the logs are durable they survive the sandbox itself.
- **One-command Linux install.** `install.sh --with-deps --enable` now provisions
  the **guest kernel** too (mirrored, pinned, and checksum-verified, with a
  firecracker-CI fallback), so `curl … | sudo bash -s -- --with-deps --enable`
  boots a working daemon end to end with nothing left to do by hand.
- **Mirrored guest kernel as a release asset.** The pinned `vmlinux-x86_64`
  (plus its `.sha256`) is published with each release for supply-chain
  independence from upstream object storage.
- Demos (`demo/*.tape` + GIFs) for `build`, `run`, `cp`, and a combined
  build → run → curl → TUI → logs hero; docs for `crucible cp`, `crucible shell`,
  the MCP file tools, and the TUI logs view. Smoke: `scripts/smoke_cp.sh`.

### Changed

- **`crucible run` long-lived hints** are aligned into a readable block instead
  of one run-on line, matching the CLI's other output.
- **Install robustness:** the rerun one-liner is correct when piped
  (`curl | sudo bash`), checkout-vs-download detection ensures a reinstall runs
  the release binary rather than a local dev build, and egress auto-wiring keys
  off the actual `CRUCIBLE_FLAGS=` line rather than matching template comments.

## [0.3.1] — 2026-07-09

**Cross-platform client + frictionless install.** The CLI, TUI, and MCP server
are thin HTTP clients over the daemon's REST API, so they now build and ship for
macOS and Windows and drive a remote Linux daemon. A reworked installer turns
setup into a one-liner for both the full daemon and a client-only install.

### Added

- **Cross-platform client binaries.** macOS and Windows client artifacts are
  built in CI and published with each release; the daemon itself stays
  Linux-only (it needs KVM + Firecracker).
- **`install.sh --client`** — install just the client (no root, no VM), pointed
  at a remote daemon with `--addr`/`--token`, with client-mode detection and
  interactive prompts for a smooth first run.
- **Release acceptance smokes** — an end-to-end acceptance smoke plus a full
  smoke runner for validating a release build.

### Changed

- **Installer hardening:** cross-platform install instructions, a warning when
  `e2fsprogs` tools are missing, and an updated config example for OCI images.
- Internal: the egress allowlist moved to a leaf package so the client
  cross-compiles without pulling in Linux-only daemon code.

## [0.3.0] — 2026-07-09

**The "safe `docker run` for untrusted code" release.** Boot an unmodified OCI
image in a Firecracker microVM in one command, poke at it with a real
interactive shell, and tear it down — with default-deny egress and
fork-for-parallel-exploration throughout. The launch line: the moment you'd
reach for `docker run` on code you don't fully trust (a random repo, something
an agent just wrote), reach for `crucible run` instead.

> **Ephemeral contract.** v0.3.0 sandboxes are *consciously ephemeral* — a
> daemon restart does **not** resurrect running sandboxes (their registry
> records and durable logs persist; the live VMs do not). That is the right
> contract for "run a sketchy repo, test it, tear it down." Durable,
> self-healing long-lived workloads are v0.4.

### Added

- **Interactive shell — `crucible shell <id>`** (and `crucible sandbox exec -i`).
  A real, long-lived `/bin/sh` into a running sandbox over a hijacked
  full-duplex vsock stream: `cd`/env persist across commands and stdin
  round-trips at interactive latency. Line-buffered, **no PTY** (full-screen
  programs, colors, and Ctrl-C job control are v0.4). Adds `FrameStdin` /
  `FrameStdinClose` to the agent wire; the one-shot `/exec` path is untouched.
- **TUI session view.** The detail view keeps a **scrollback** of every command
  in a session, and **`tab`** opens the interactive shell (from the list, or
  inside detail) so commands share state — riding the same `ExecInteractive`.
- **`crucible run <image>`** — the docker-parity headline: acquire → boot the
  image's entrypoint → publish ports (`-p`) → **long-lived by default**. `--rm`
  tails logs in the foreground and removes the sandbox on Ctrl-C. The previous
  throwaway-command shape stays as `crucible run -- <command>`.
- **`crucible build [-t tag] [-f Dockerfile] <context>`** — `docker build`
  locally, then load the result into crucible's image store in one verb (Docker
  stays client-side; the daemon is Docker-free). Prints the converted digest,
  ready for `crucible run`.
- **`crucible stop <id>` / `crucible rm <id>`** — top-level ops verbs: graceful
  stop (image StopSignal + grace, the sandbox remains) and hard remove.
- **`--disk <size>`** on `sandbox create` / `run` — grow the writable rootfs
  (e.g. `2G`, `512M`) by resizing the *per-sandbox clone* (`resize2fs`) before
  boot; the shared image/profile ext4 is never touched.
- **MCP surface for the wedge.** `create_sandbox` / `run` gain `image` + `pull`,
  `disk_mib`, and (create) `publish`; two new tools — **`logs`** (durable
  service/exec logs that survive the sandbox) and **`stop_sandbox`** (graceful
  stop). See [docs/mcp.md](docs/mcp.md).
- Smokes: `scripts/smoke_shell.sh`, `smoke_reap.sh`, `smoke_build_run.sh`.

### Changed

- **`crucible run` is dual-mode**, selected by the `--` separator: a bare
  positional is an *image* (`run nginx -p 8080:80`); `-- <cmd>` is a *throwaway
  command* (the prior behavior, unchanged).
- **Ctrl-C is graceful across the CLI.** Client commands cancel their context on
  SIGINT/SIGTERM instead of being hard-killed, so `run --rm` cleans up, `logs
  -f` stops cleanly, and an interactive `shell`/`exec -i` tears the guest
  process down on exit.

### Fixed

- **Bare commands resolve on OCI images.** The PID-1 (init-mode) agent spawns via
  `os.StartProcess`, which does no `PATH` search — so `sh` / `sh -c` (the TUI,
  `exec -- sh`, `shell`) failed to start on image sandboxes with `exit -1`. The
  init exec path now resolves `argv[0]` against the child's `PATH`, matching
  profile mode and Docker.
- **Orphan reaping is complete.** A killed daemon leaves no lingering
  firecracker: startup now sweeps live orphan **processes** (scoped by
  `--chroot-base-dir`) alongside the existing chroot-driven reap, and mops up
  empty orphan **cgroup directories** whose chroot is already gone (these
  previously accumulated indefinitely).

### Security

- **Scope stated honestly.** v0.3.0 supports *running code you distrust on your
  own host*; it does **not** yet support hosting mutually-distrusting tenants on
  one host — that is gated on a hardening + external-audit pass. The microVM +
  jailer + default-deny-egress boundary is described precisely in
  [SECURITY.md](SECURITY.md).
- MCP image / publish / disk params pass through under the **existing** operator
  guardrails (timeout / net-allow / fork / sandbox clamps) — no new
  agent-widenable capability, and the resolved-IP range filter still gates all
  egress.

## [0.2.0] — 2026-07-08

The TUI release: a live terminal control center for a crucible daemon — see
running sandboxes, the fork tree, and interactive streaming `exec` at a glance,
and drive create / snapshot / fork / delete without leaving the dashboard. Like
the CLI and MCP server it's a thin consumer of the same typed client
(`internal/client`), so a dashboard action and a CLI command hit the exact same
API path and can't drift.

### Added

- **`crucible tui`** — a [Bubble Tea](https://github.com/charmbracelet/bubbletea)
  dashboard that polls the daemon and renders live: a sandbox table (id, profile,
  age, CPU/memory, network, fork mark), a **fork-tree view** (`t`) built from the
  sandbox + snapshot genealogy, and a **detail + exec view** (`enter`) with a
  scrolling viewport where a command's stdout/stderr stream live and finish with
  an exit chip. Connects with the usual `--addr`/`--token`/`--tls-skip-verify`;
  the header shows the token's scope when the daemon reports one. See
  [docs/tui.md](docs/tui.md).
- **Actions with scope-gating.** Create (`c`), snapshot (`s`), fork (`f`, from
  the selected sandbox's latest snapshot), and delete (`d`, with a `y`/`n`
  confirm) run as async calls with status-line feedback. Each is gated on the
  token's policy — forbidden operations are struck through in the hint and
  rejected on keypress, mirroring what the daemon enforces authoritatively. The
  layout is responsive: hints compact on a narrow terminal and nothing overflows.
- **Fork lineage on the API.** `SandboxResponse` gains `source_snapshot_id` (the
  snapshot a sandbox was forked from, stamped in `forkOne`), so the fork
  genealogy is reconstructable by any client — this is what the tree view draws.
- **`crucible daemon --max-fork`** (env `CRUCIBLE_MAX_FORK`) — bound how many
  sandboxes a single fork request may create (0 = the built-in default of 64). A
  scoped token's own `max_fork` can still only tighten it.
- **Benchmark harness** — `cmd/crucible-bench` (`make bench`) drives a real
  daemon through `internal/client` and reports latency distributions, fork
  fan-out scaling, lazy-memory efficiency, and density; see
  [docs/benchmarks.md](docs/benchmarks.md).
- `docs/tui.md` and `demo/tui.tape`, `demo/network.tape`, `demo/bench.tape`
  [vhs](https://github.com/charmbracelet/vhs) scripts for regenerating the demo
  GIFs.

### Changed

- `VISION.md` observability wording is now precise: per-exec results (with
  CPU/memory/I/O usage) are **returned to the caller, not yet persisted** —
  durable per-sandbox activity logs are called out as the next step rather than
  implied to ship today.

## [0.1.3] — 2026-07-07

Scoped / policy tokens: bind an API key to a policy the **daemon** enforces, so a
leaked or handed-out key is worthless beyond its policy — the fix that makes the
MCP guardrails a real boundary and remote/hosted access genuinely safe.

### Added

- **Scoped tokens.** `crucible daemon token add --policy <file> --ttl <dur>`
  mints a key bound to a JSON policy — allowed operations, an egress ceiling, a
  profile allowlist, and resource caps (sandboxes / fork / timeout / vCPU /
  memory) — with an optional expiry. The daemon enforces the policy on every
  request; an unscoped key keeps full access, so nothing regresses. See
  [docs/policy.md](docs/policy.md).
- **`crucible policy validate <file|->`** — the *same* validation that gates
  `token add` (fail-closed), reporting every problem at once.
- **`crucible policy show` / `GET /whoami`** — the effective policy for the
  presenting token, so a client can discover exactly what it may do.
- **MCP tool mirror.** `mcp serve` asks `/whoami` at startup and advertises only
  the tools the token's policy permits (the daemon still enforces regardless).
- `docs/policy.md`, plus a scoped-token check in `scripts/smoke_mcp.sh`.

### Changed

- `daemon token list` now shows each key's scope (full/scoped) and expiry.
- The MCP server's `--max-*` / `--net-allow-max` flags are **narrow-only**: they
  layer on top of the daemon-enforced token policy and can only tighten it.

### Security

- **The local same-user bypass is closed for scoped tokens.** Enforcement is
  daemon-authoritative, so an agent that steals its MCP server's scoped key and
  calls the loopback daemon directly gets only the bounded capability it already
  had. Expired tokens are rejected (`401`), and `max_sandboxes` is counted
  per-token so tokens don't share a budget. (OAuth-style short-lived session
  tokens — an MCP server exchanging its key for a scoped session token — are
  deferred.)

## [0.1.2] — 2026-07-07

The MCP release: drive crucible from any MCP agent as native tools, and reach a
remote/hosted daemon safely with API-key auth.

### Added

- **MCP server — `crucible mcp serve`.** A stdio [Model Context
  Protocol](https://modelcontextprotocol.io) server so any MCP agent (Claude
  Code, Cursor, …) can drive crucible as native tools. It is a thin client of
  the daemon (built on the official Go MCP SDK), so an MCP tool call and the
  equivalent CLI command hit the identical code path and can't drift. Full tool
  catalog: `run` (create → exec → delete in one call), `create_sandbox`,
  `exec`, `snapshot`, `fork`, `list_sandboxes`, `inspect_sandbox`,
  `delete_sandbox`, `list_snapshots`, `delete_snapshot`, `list_profiles`.
  Because the server is a client, `--addr` can point at a remote daemon — the
  same local stdio subprocess bridges to it. See [docs/mcp.md](docs/mcp.md).
- **MCP operator guardrails.** The operator fixes policy at launch; the agent
  can never widen it. Network stays default-deny; `--net-allow-max` caps
  agent-chosen egress; `--max-sandboxes` (8), `--max-fork` (8), and
  `--max-timeout` (300s) bound resources; `--default-profile` /
  `--allow-profiles` and `--tools` / `--deny-tools` reduce the surface (a
  filtered tool never appears in `tools/list`).
- **Daemon API-key authentication.** Bearer keys on the docker/kubectl model:
  `crucible daemon token add` / `list` / `revoke` (rotate = add + revoke). Keys
  are stored as SHA-256 hashes, so a leaked token file yields no usable keys.
  The daemon checks `Authorization: Bearer <key>` on every request (`/healthz`
  exempt); auth turns on automatically once any key exists. Clients pass a key
  with `--token` or `CRUCIBLE_TOKEN`.
- **TLS for the daemon** — `--tls-cert` / `--tls-key`, with `--tls-skip-verify`
  on the client for self-signed dev certs.
- **`scripts/smoke_mcp.sh`** — integration smoke that drives `crucible mcp
  serve` against a real daemon (tool catalog, `run` round-trip, a guardrail
  rejection, the auth path) and guards against VM/cgroup leaks.
- Docs: [docs/mcp.md](docs/mcp.md), an MCP threat-model section in
  [SECURITY.md](SECURITY.md), and auth/MCP notes across
  [docs/api.md](docs/api.md), [docs/cli.md](docs/cli.md), and
  `packaging/crucible.env.example`.

### Changed

- **`--jail-gid` now defaults to the `kvm` group** so the jailed firecracker
  can open `/dev/kvm` out of the box (it reaches the device through its group).
  Falls back to `10000` on hosts without a kvm group; an explicit `--jail-gid`
  still wins. This removes a cryptic first-run `creating KVM object: Permission
  denied` in jailer mode.

### Fixed

- **VM leak on jailer-mode delete.** A sandbox delete (and `run`/fork cleanup)
  signalled only the jailer process; with `--new-pid-ns` the firecracker child
  lives in its own PID namespace and was orphaned to init — a leaked microVM
  that also kept its cgroup populated (the `jailer cleanup failed … device or
  resource busy` warning). Teardown now kills the whole VM process set (jailer
  **and** firecracker, by their `--id` cmdline token) and waits for it to drain
  before removing the chroot and cgroup. The cgroup `rmdir` additionally
  retries the brief post-exit window during which cgroupfs still reports the
  group populated.

### Security

- Daemon API-key auth (above) provides remote/hosted access control and
  per-client revocation. Binding a **non-loopback** address is refused unless
  both API keys and TLS are configured — a bearer token over plaintext HTTP is
  a leaked token.
- MCP guardrails bound what a (potentially prompt-injected) agent can do; the
  daemon's default-deny networking and resolved-IP range filter still apply
  regardless of an agent-chosen allowlist, so agent egress can never reach
  cloud-metadata or internal addresses.
- **Known limitation:** a local, same-OS-user agent with a shell tool can read
  the daemon key and bypass the MCP guardrails by calling the loopback daemon
  directly. Bearer keys make the *remote* story solid but don't bound this; the
  planned fix is daemon-enforced **scoped tokens** (deferred). See
  [docs/mcp.md#limitations](docs/mcp.md#limitations).

## [0.1.1] — 2026-07-07

### Fixed

- Guest networking on the native rootfs profiles: install `udev` + `dbus` and
  write `/etc/resolv.conf` so DNS resolves inside the guest.
- Installer: keep the daemon's work-base persistent (off `/tmp`, so the
  registry survives reboot), quiet the installer output, and complete the
  install docs.

## [0.1.0] — 2026-07-07

Initial release — the core single-host Firecracker microVM sandbox runtime.

### Added

- Firecracker microVM runtime with **jailer isolation** (per-VM chroot + mount/
  PID namespaces + privilege drop) and host cgroup v2 quotas.
- **Snapshot / fork** with lazy guest memory via `userfaultfd` (fork serves
  page faults from the snapshot's memory file instead of byte-copying RAM).
- **Clone-safety**: each fork's kernel RNG is reseeded and machine identifiers
  rotated before it can be exec'd, so no two forks wake sharing RNG/UUIDs.
- **Per-sandbox networking**, default-deny: own netns/veth/tap, per-netns DHCP,
  a DNS proxy that only resolves allowlisted hostnames, and a resolved-IP range
  filter (blocks link-local / RFC1918 / CGNAT, closing SSRF to cloud metadata).
- **Durable registry + reconcile-on-restart**: sandbox/snapshot records are
  journaled; on startup snapshots are re-adopted and orphaned state is reaped.
- CLI over the REST API (`sandbox`, `snapshot`, `fork`, `profile`, `run`;
  `-o json`), native language rootfs profiles (base/python/node/go), a
  Prometheus `/metrics` endpoint, and an install script + systemd unit.

[0.3.2]: https://github.com/gnana997/crucible/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/gnana997/crucible/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/gnana997/crucible/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/gnana997/crucible/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/gnana997/crucible/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/gnana997/crucible/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/gnana997/crucible/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/gnana997/crucible/releases/tag/v0.1.0
