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
//	…with an optional /tcp suffix (tcp is the default).
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
	parts := strings.Split(body, ":")
	var hostStr, guestStr string
	switch len(parts) {
	case 2:
		hostStr, guestStr = parts[0], parts[1]
	case 3:
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
