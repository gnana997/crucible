package daemon

import (
	"testing"

	"github.com/gnana997/crucible/internal/oci"
	"github.com/gnana997/crucible/sdk/api"
)

func TestExposedPortPublish(t *testing.T) {
	t.Run("nil runconfig or no exposed ports yields nothing", func(t *testing.T) {
		if got, _ := exposedPortPublish(nil, nil); got != nil {
			t.Fatalf("nil rc: got %v, want nil", got)
		}
		if got, _ := exposedPortPublish(&oci.RunConfig{}, nil); got != nil {
			t.Fatalf("no exposed ports: got %v, want nil", got)
		}
	})

	t.Run("expands tcp to guest N -> host N, skips udp", func(t *testing.T) {
		rc := &oci.RunConfig{ExposedPorts: []string{"80/tcp", "53/udp", "8080"}}
		got, err := exposedPortPublish(rc, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// 80/tcp and bare 8080 (tcp) expand; 53/udp is skipped (publish is tcp-only).
		if len(got) != 2 {
			t.Fatalf("got %d mappings, want 2: %+v", len(got), got)
		}
		for _, m := range got {
			if m.HostPort != m.GuestPort || m.Protocol != "tcp" {
				t.Errorf("mapping %+v: want host==guest and tcp", m)
			}
		}
	})

	t.Run("explicit publish for a guest port wins", func(t *testing.T) {
		rc := &oci.RunConfig{ExposedPorts: []string{"80/tcp", "443/tcp"}}
		explicit := []api.PortMapping{{HostPort: 8080, GuestPort: 80, Protocol: "tcp"}}
		got, err := exposedPortPublish(rc, explicit)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// 80 is already mapped explicitly; only 443 should be auto-added.
		if len(got) != 1 || got[0].GuestPort != 443 {
			t.Fatalf("got %+v, want a single mapping for guest 443", got)
		}
	})

	t.Run("bad exposed port is an error", func(t *testing.T) {
		if _, err := exposedPortPublish(&oci.RunConfig{ExposedPorts: []string{"notaport/tcp"}}, nil); err == nil {
			t.Fatal("want error for a non-numeric exposed port")
		}
	})
}
