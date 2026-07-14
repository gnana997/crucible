package api

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePublish parses a docker-style port publish spec into a PortMapping:
//
//	HOST:GUEST              8080:80
//	HOST_IP:HOST:GUEST      127.0.0.1:8080:80
//	[V6_IP]:HOST:GUEST      [::1]:8080:80
//	…with an optional /tcp suffix (tcp is the default).
//
// An IPv6 bind address must be bracketed (docker's syntax too) — a bare v6
// literal is ambiguous with the port separators.
//
// Shared by the CLI (`--publish`) and the MCP server so the two surfaces
// can't diverge on the accepted shape.
func ParsePublish(spec string) (PortMapping, error) {
	var pm PortMapping
	body, proto, hasProto := strings.Cut(spec, "/")
	pm.Protocol = "tcp"
	if hasProto {
		pm.Protocol = proto
	}
	if strings.HasPrefix(body, "[") {
		v6, rest, found := strings.Cut(body[1:], "]")
		if !found || !strings.HasPrefix(rest, ":") {
			return pm, fmt.Errorf("publish %q: want [HOST_IP:]HOST:GUEST[/tcp]", spec)
		}
		pm.HostIP = v6
		body = rest[1:]
	}
	parts := strings.Split(body, ":")
	var hostStr, guestStr string
	switch {
	case len(parts) == 2:
		hostStr, guestStr = parts[0], parts[1]
	case len(parts) == 3 && pm.HostIP == "":
		pm.HostIP, hostStr, guestStr = parts[0], parts[1], parts[2]
	default:
		return pm, fmt.Errorf("publish %q: want [HOST_IP:]HOST:GUEST[/tcp]", spec)
	}
	hp, err := strconv.Atoi(hostStr)
	if err != nil {
		return pm, fmt.Errorf("publish %q: bad host port %q", spec, hostStr)
	}
	gp, err := strconv.Atoi(guestStr)
	if err != nil {
		return pm, fmt.Errorf("publish %q: bad guest port %q", spec, guestStr)
	}
	pm.HostPort, pm.GuestPort = hp, gp
	return pm, nil
}
