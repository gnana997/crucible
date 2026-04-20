// Package fcapi is a minimal, hand-written client for the Firecracker
// VMM HTTP API.
//
// Firecracker exposes its management API as HTTP/1.1 over a unix-domain
// socket. The socket path is chosen when you spawn the VMM with
// `firecracker --api-sock PATH`. Every request/response is JSON; PUTs
// return 204 No Content on success, GETs return 200 with a JSON body, and
// errors come back as 4xx with {"fault_message": "..."} in the body.
//
// # Boot sequence
//
// Getting from "just spawned firecracker" to "guest is running" takes
// four API calls, in order:
//
//  1. GET /                      → readiness probe; expect state "Not started".
//  2. PUT /boot-source           → guest kernel path + kernel command line.
//  3. PUT /drives/{drive_id}     → guest root block device (rootfs.ext4).
//  4. PUT /machine-config        → vcpu count, memory.
//  5. PUT /actions {InstanceStart} → power on.
//
// Optional devices (configured between step 4 and step 5):
//   - PUT /vsock                  → virtio-vsock channel + host UDS path.
//   - PUT /network-interfaces/{id} → TAP device + guest MAC.
//
// # Scope discipline
//
// This package implements only the endpoints crucible currently needs.
// The upstream OpenAPI spec covers many more (CPU templates, MMDS,
// balloon, rate-limiters, metrics config, snapshot create/load). We add
// methods only when a crucible feature requires them, to keep the client
// small enough that a reader can hold the whole thing in their head.
//
// # References
//
// Upstream API spec (version-pinned to whichever Firecracker we test
// against):
//
//	https://github.com/firecracker-microvm/firecracker/blob/v1.15.1/src/firecracker/swagger/firecracker.yaml
//
// Getting-started guide with a worked boot example:
//
//	https://github.com/firecracker-microvm/firecracker/blob/v1.15.1/docs/getting-started.md
package fcapi
