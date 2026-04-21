//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"time"
)

// networkRefreshTimeout bounds the full down→up→wait dance. Long
// enough to cover a DHCP server that takes a moment to respond,
// short enough that a wedged interface doesn't hold the host's
// Fork call hostage.
const networkRefreshTimeout = 10 * time.Second

// guestIface is the single network interface we manage. Firecracker
// always exposes its configured NIC as eth0 inside the guest, so
// this is stable across all rootfs profiles.
const guestIface = "eth0"

// ipLinkCmd is the iproute2 executable we shell out to. `ip` is
// part of iproute2, which has been in every Ubuntu/Debian base for
// decades — no extra package needed.
const ipLinkCmd = "ip"

// waitPollInterval is how often we re-check whether eth0 has
// gained an IPv4 address after the link-up. 100 ms is short
// enough for a quick DHCP round-trip to show up promptly without
// hammering the kernel.
const waitPollInterval = 100 * time.Millisecond

// ipLinkRunner executes `ip link set <args>`. Swappable at package
// scope so tests can substitute a stub that records calls instead
// of actually manipulating interfaces.
var ipLinkRunner = runIPLink

// waitForIP returns once ifaceName has a non-link-local,
// non-loopback IPv4 address, or the context is done. Swappable
// for tests so we don't need a real interface with a real DHCP
// server during unit tests.
var waitForIP = waitForIfaceIPv4

// runIPLink is the production implementation of ipLinkRunner.
// Captures stdout+stderr for the error message on failure.
func runIPLink(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, ipLinkCmd, append([]string{"link", "set"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link set %v: %w: %s", args, err, out)
	}
	return nil
}

// waitForIfaceIPv4 polls net.InterfaceByName until the named
// interface has an IPv4 address assigned, or ctx is canceled.
// Read via netlink under the hood — we stay off the `ip` CLI
// here to avoid spawning a subprocess per poll tick.
//
// "Usable" means: IPv4, not loopback, not link-local (169.254/16).
// DHCP-assigned addresses are neither, so this predicate fires
// exactly when systemd-networkd has finished configuring eth0.
func waitForIfaceIPv4(ctx context.Context, ifaceName string) error {
	for {
		if hasUsableIPv4(ifaceName) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// hasUsableIPv4 is waitForIfaceIPv4's single-shot check, split out
// so the polling loop stays readable.
func hasUsableIPv4(ifaceName string) bool {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		if ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
			continue
		}
		return true
	}
	return false
}

// handleNetworkRefresh bounces eth0 so systemd-networkd (the
// DHCP client shipped with modern Ubuntu/Debian) triggers a fresh
// DHCP cycle and picks up the fork's assigned IP. Three steps:
//
//  1. `ip link set eth0 down` — kernel flushes the interface's
//     address config and sends a link-down event.
//  2. `ip link set eth0 up` — interface comes back; systemd-
//     networkd sees the link-up event and starts a new DHCP
//     session from scratch (DISCOVER, not a REQUEST of the
//     stale lease).
//  3. Poll net.InterfaceByName until eth0 has a non-link-local
//     IPv4 address. This is the "network is usable" signal.
//
// Called by sandbox.Manager.Fork post-resume. On a forked VM
// that restored from snapshot, eth0 still has the source VM's
// IP from snapshot time; bouncing the link lets systemd-networkd
// renegotiate with our per-netns DHCP responder and land on the
// fork's correct address within a few hundred milliseconds
// instead of waiting for a renewal-timer cycle.
//
// All three steps are fatal on failure — there's no graceful
// fallback if the link can't be bounced or the DHCP handshake
// doesn't complete. The caller (host-side Manager.Fork) logs
// the error and continues with fork: the guest will still boot,
// it just won't have network until something else restores it.
func handleNetworkRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), networkRefreshTimeout)
	defer cancel()

	if err := ipLinkRunner(ctx, guestIface, "down"); err != nil {
		slog.Error("ip link down failed", "iface", guestIface, "err", err)
		http.Error(w,
			fmt.Sprintf("network refresh failed (down): %v", err),
			http.StatusInternalServerError,
		)
		return
	}

	if err := ipLinkRunner(ctx, guestIface, "up"); err != nil {
		slog.Error("ip link up failed", "iface", guestIface, "err", err)
		http.Error(w,
			fmt.Sprintf("network refresh failed (up): %v", err),
			http.StatusInternalServerError,
		)
		return
	}

	if err := waitForIP(ctx, guestIface); err != nil {
		// Distinguish "we timed out waiting" from "context got
		// canceled by the caller" in the logged message — both
		// look the same from waitForIP's return, but the user-
		// visible status is the same either way (500).
		reason := "wait for IPv4 timed out"
		if errors.Is(err, context.Canceled) {
			reason = "caller canceled during wait"
		}
		slog.Error("ip wait failed", "iface", guestIface, "err", err, "reason", reason)
		http.Error(w,
			fmt.Sprintf("network refresh failed (%s): %v", reason, err),
			http.StatusInternalServerError,
		)
		return
	}

	slog.Info("network refreshed", "iface", guestIface)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
