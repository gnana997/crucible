//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/vishvananda/netlink"

	"github.com/gnana997/crucible/internal/agentwire"
)

// maxNetConfigBody bounds the POST /network/configure body — a small
// JSON blob (an address, gateway, DNS, hostname).
const maxNetConfigBody = 1 << 20

// netconfigApplier is a seam for tests: the real implementation drives
// netlink and needs a real interface + root.
var netconfigApplier = applyNetConfig

// resolvConfPath / hostsPath are package vars so tests can redirect
// them into a temp dir. (hostnamePath is shared with identity.go.)
var (
	resolvConfPath = "/etc/resolv.conf"
	hostsPath      = "/etc/hosts"
)

// handleNetworkConfigure applies a host-pushed static network
// configuration via netlink. This is the OCI-guest counterpart to
// handleNetworkRefresh (DHCP link-bounce): an image guest has no DHCP
// client, so the daemon sends the address it already allocated and the
// agent programs eth0 directly. Idempotent — safe to call again on
// fork with a new address.
func handleNetworkConfigure(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxNetConfigBody)
	var req agentwire.NetworkConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("network configure failed (decode): %v", err), http.StatusBadRequest)
		return
	}
	if req.Interface == "" {
		req.Interface = guestIface
	}
	if req.Address == "" || req.PrefixLen <= 0 || req.PrefixLen > 32 {
		http.Error(w, "network configure failed: address and a 1..32 prefix_len are required", http.StatusBadRequest)
		return
	}

	if err := netconfigApplier(&req); err != nil {
		slog.Error("network configure failed", "iface", req.Interface, "err", err)
		http.Error(w, fmt.Sprintf("network configure failed: %v", err), http.StatusInternalServerError)
		return
	}
	if err := writeResolverFiles(&req); err != nil {
		slog.Error("network configure: resolver files failed", "err", err)
		http.Error(w, fmt.Sprintf("network configure failed (resolver): %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("network configured", "iface", req.Interface, "address", req.Address, "gateway", req.Gateway)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// applyNetConfig programs the interface via netlink: bring it up,
// replace its IPv4 address (flushing any stale addresses from a
// snapshot), and set the default route. No external `ip` binary — the
// point of this path is guests that don't ship one.
func applyNetConfig(req *agentwire.NetworkConfigRequest) error {
	link, err := netlink.LinkByName(req.Interface)
	if err != nil {
		return fmt.Errorf("find link %s: %w", req.Interface, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link up %s: %w", req.Interface, err)
	}

	// Flush existing IPv4 addresses so a fork's restored-from-snapshot
	// address doesn't linger alongside the new one.
	existing, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list addrs %s: %w", req.Interface, err)
	}
	for i := range existing {
		if err := netlink.AddrDel(link, &existing[i]); err != nil {
			return fmt.Errorf("flush addr %s: %w", existing[i].IPNet, err)
		}
	}

	addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", req.Address, req.PrefixLen))
	if err != nil {
		return fmt.Errorf("parse address %s/%d: %w", req.Address, req.PrefixLen, err)
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("set address: %w", err)
	}

	if req.Gateway != "" {
		gw := net.ParseIP(req.Gateway)
		if gw == nil {
			return fmt.Errorf("invalid gateway %q", req.Gateway)
		}
		// Default route (Dst nil) via the gateway, on this link.
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("set default route via %s: %w", req.Gateway, err)
		}
	}
	return nil
}

// writeResolverFiles writes /etc/resolv.conf, /etc/hosts, and the
// hostname (kernel + /etc/hostname). Best-effort about the guest's
// existing /etc layout: resolv.conf is often a symlink, so it is
// replaced (unlink + write), and a missing /etc is created.
func writeResolverFiles(req *agentwire.NetworkConfigRequest) error {
	if err := os.MkdirAll("/etc", 0o755); err != nil {
		return err
	}
	if len(req.DNS) > 0 {
		var b strings.Builder
		for _, ns := range req.DNS {
			fmt.Fprintf(&b, "nameserver %s\n", ns)
		}
		// resolv.conf is commonly a symlink (e.g. to systemd-resolved);
		// remove it first so we write a real file, not through the link.
		_ = os.Remove(resolvConfPath)
		if err := os.WriteFile(resolvConfPath, []byte(b.String()), 0o644); err != nil {
			return fmt.Errorf("write resolv.conf: %w", err)
		}
	}
	if req.Hostname != "" {
		if err := hostnameSetter([]byte(req.Hostname)); err != nil {
			return fmt.Errorf("sethostname: %w", err)
		}
		_ = os.WriteFile(hostnamePath, []byte(req.Hostname+"\n"), 0o644)
		hosts := fmt.Sprintf("127.0.0.1\tlocalhost\n%s\t%s\n", req.Address, req.Hostname)
		if err := os.WriteFile(hostsPath, []byte(hosts), 0o644); err != nil {
			return fmt.Errorf("write hosts: %w", err)
		}
	}
	return nil
}
