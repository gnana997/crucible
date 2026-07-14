package reuseport

import (
	"net"
	"testing"
)

func dials(t *testing.T, network, addr string) bool {
	t.Helper()
	c, err := net.Dial(network, addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func serve(t *testing.T, ln net.Listener) {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

func v6Loopback(t *testing.T) bool {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// The edge's IPv6 story rests on wildcard "tcp" binds being dual-stack: the
// ingress proxy, waking forwarder, and port publish all listen this way, so
// ":80"-style addresses must accept both families without any v6-specific
// code. Pin that here — a kernel/runtime where it doesn't hold (e.g.
// net.ipv6.bindv6only=1) should fail loudly, not silently drop v6.
func TestWildcardListenIsDualStack(t *testing.T) {
	if !v6Loopback(t) {
		t.Skip("no IPv6 loopback on this host")
	}
	ln, err := Listen(":0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serve(t, ln)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if !dials(t, "tcp4", "127.0.0.1:"+port) {
		t.Error("wildcard listener not reachable over IPv4")
	}
	if !dials(t, "tcp6", "[::1]:"+port) {
		t.Error("wildcard listener not reachable over IPv6 (dual-stack broken)")
	}
}

// A publish spec pinned to a v6 address ("[::1]:8080:80") binds v6-only —
// an explicit address is the operator's choice, never mirrored to v4.
func TestSpecificV6BindIsV6Only(t *testing.T) {
	if !v6Loopback(t) {
		t.Skip("no IPv6 loopback on this host")
	}
	ln, err := Listen("[::1]:0")
	if err != nil {
		t.Fatalf("Listen([::1]:0): %v", err)
	}
	serve(t, ln)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if !dials(t, "tcp6", "[::1]:"+port) {
		t.Error("v6-pinned listener not reachable over IPv6")
	}
	if dials(t, "tcp4", "127.0.0.1:"+port) {
		t.Error("v6-pinned listener unexpectedly reachable over IPv4")
	}
}
