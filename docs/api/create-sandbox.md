---
title: Create a sandbox
description: "POST /sandboxes field by field: sizing, profiles vs images, port publishing, disk growth, and the network policy rules."
openapi: "POST /sandboxes"
---

All fields are optional: an empty body `{}` boots a sandbox with daemon defaults.

```json
{
  "vcpus": 1,
  "memory_mib": 512,
  "boot_args": "",
  "timeout_s": 0,
  "network": {
    "enabled": true,
    "allowlist": ["pypi.org", "*.npmjs.org"]
  }
}
```

| Field | Type | Notes |
|---|---|---|
| `vcpus` | int | vCPU count. Defaulted and range-capped by the daemon. |
| `memory_mib` | int | Guest memory in MiB. Defaulted and range-capped. |
| `boot_args` | string | Extra kernel cmdline appended to the daemon's default. |
| `timeout_s` | int | Max sandbox lifetime in seconds. `0` = no timeout (lives until `DELETE` or shutdown). |
| `profile` | string | Pre-baked rootfs to boot from, e.g. `"python-3.12"`. Empty uses the daemon's default `--rootfs`. An unknown profile is a `400`. See [Profiles](../profiles.md). |
| `network` | object | Omit or `null` for no network (default-deny). See [Networking](../network.md). |
| `image` | object | Boot from an OCI image instead of a profile: `{"oci": "nginx:alpine"}` (a registry ref or a converted digest). Mutually exclusive with `profile`. |
| `pull` | string | Image pull policy when `image.oci` is set: `"missing"` (default), `"always"`, `"never"`. |
| `publish` | array | Host-to-guest port publishes, e.g. `[{"host_port":8080,"guest_port":80}]` (optional `host_ip`, default `protocol` `"tcp"`). Requires a NIC; when `network` is absent one is created ingress-published, egress-denied. |
| `disk_bytes` | int | Grow the writable rootfs clone to at least this size before boot. `0` keeps the image or profile headroom; a smaller value is a no-op (never shrinks). The shared template is never modified. |

> [!IMPORTANT]
> When `network` is present, `enabled: true` requires a non-empty `allowlist`; unrestricted full-internet egress must be requested explicitly (see [Networking](../network.md)). `enabled: false` with a populated allowlist is a `400`.

The `201` response echoes the applied config with the assigned addresses; `network` is omitted when the sandbox has no NIC, and `profile` is echoed when the sandbox booted from a named profile.
