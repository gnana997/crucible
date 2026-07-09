// Package oci turns OCI/Docker container images into crucible rootfs
// artifacts. It covers the image-conversion pipeline: acquire (anonymous
// registry pull or docker-save side-load), assemble (flatten layers with
// whiteout handling, harden, inject the agent + run.json), materialize
// (ext4 via mkfs tar-mode or a staging-dir fallback), and store
// (content-addressed cache).
//
// Boot-time consumption of these artifacts is a separate concern: the daemon computes
// the effective service spec from the same OCI config recorded here
// and pushes it over vsock; the in-image run.json is an offline
// debugging record, not the live path.
//
// This package is daemon-side only. The guest agent must never import
// it (it would drag go-containerregistry into the static agent binary).
package oci
