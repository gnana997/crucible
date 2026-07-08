package daemon

import (
	"strings"
	"testing"

	"github.com/gnana997/crucible/internal/api"
)

func TestValidatePublish(t *testing.T) {
	cases := []struct {
		name    string
		in      []api.PortMapping
		wantErr string // substring; empty = valid
	}{
		{
			name: "valid tcp default",
			in:   []api.PortMapping{{HostPort: 8080, GuestPort: 80}},
		},
		{
			name: "valid with host ip and proto",
			in:   []api.PortMapping{{HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80, Protocol: "tcp"}},
		},
		{
			name:    "udp rejected",
			in:      []api.PortMapping{{HostPort: 53, GuestPort: 53, Protocol: "udp"}},
			wantErr: "not supported",
		},
		{
			name:    "host port zero",
			in:      []api.PortMapping{{HostPort: 0, GuestPort: 80}},
			wantErr: "host_port",
		},
		{
			name:    "guest port too big",
			in:      []api.PortMapping{{HostPort: 8080, GuestPort: 70000}},
			wantErr: "guest_port",
		},
		{
			name: "duplicate host port",
			in: []api.PortMapping{
				{HostPort: 8080, GuestPort: 80},
				{HostPort: 8080, GuestPort: 81},
			},
			wantErr: "more than once",
		},
		{
			name: "same host port different ip is ok",
			in: []api.PortMapping{
				{HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80},
				{HostIP: "10.0.0.1", HostPort: 8080, GuestPort: 80},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := validatePublish(tc.in)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validatePublish = %v, want nil", err)
				}
				if len(out) != len(tc.in) {
					t.Errorf("got %d mappings, want %d", len(out), len(tc.in))
				}
				for i := range out {
					if out[i].Protocol == "" {
						t.Errorf("protocol not defaulted on mapping %d", i)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validatePublish = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
