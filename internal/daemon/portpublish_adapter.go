package daemon

import (
	"context"
	"log/slog"

	"github.com/gnana997/crucible/internal/portpublish"
	"github.com/gnana997/crucible/internal/sandbox"
)

// portPublisher adapts internal/portpublish to the narrow
// sandbox.PortPublisher interface, mapping the sandbox-layer port list
// onto the forwarder's Mapping (which also needs the guest IP). Lives
// in the daemon package for the same reason the network adapter does:
// internal/sandbox must not open host net.Listeners itself.
type portPublisher struct {
	log *slog.Logger
}

// NewPortPublisher returns a sandbox.PortPublisher backed by
// internal/portpublish.
func NewPortPublisher(log *slog.Logger) sandbox.PortPublisher {
	if log == nil {
		log = slog.Default()
	}
	return &portPublisher{log: log}
}

func (p *portPublisher) Publish(_ context.Context, sandboxID, guestIP string, ports []sandbox.PortMapping) (sandbox.PublishHandle, error) {
	mappings := make([]portpublish.Mapping, 0, len(ports))
	for _, m := range ports {
		mappings = append(mappings, portpublish.Mapping{
			HostIP:    m.HostIP,
			HostPort:  m.HostPort,
			GuestIP:   guestIP,
			GuestPort: m.GuestPort,
		})
	}
	set, err := portpublish.Publish(p.log.With("sandbox", sandboxID), mappings)
	if err != nil {
		return nil, err
	}
	return set, nil
}
