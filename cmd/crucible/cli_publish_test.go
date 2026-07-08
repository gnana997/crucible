package main

import "testing"

func TestParsePublish(t *testing.T) {
	cases := []struct {
		spec                        string
		wantIP, wantProto           string
		wantHostPort, wantGuestPort int
		wantErr                     bool
	}{
		{spec: "8080:80", wantHostPort: 8080, wantGuestPort: 80, wantProto: "tcp"},
		{spec: "8080:80/tcp", wantHostPort: 8080, wantGuestPort: 80, wantProto: "tcp"},
		{spec: "127.0.0.1:8080:80", wantIP: "127.0.0.1", wantHostPort: 8080, wantGuestPort: 80, wantProto: "tcp"},
		{spec: "127.0.0.1:8080:80/tcp", wantIP: "127.0.0.1", wantHostPort: 8080, wantGuestPort: 80, wantProto: "tcp"},
		{spec: "80", wantErr: true},
		{spec: "a:b", wantErr: true},
		{spec: "8080:80:extra:more", wantErr: true},
		{spec: "8080:notaport", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			pm, err := parsePublish(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePublish(%q) = %+v, want error", tc.spec, pm)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePublish(%q): %v", tc.spec, err)
			}
			if pm.HostIP != tc.wantIP || pm.HostPort != tc.wantHostPort ||
				pm.GuestPort != tc.wantGuestPort || pm.Protocol != tc.wantProto {
				t.Errorf("parsePublish(%q) = %+v, want ip=%q host=%d guest=%d proto=%q",
					tc.spec, pm, tc.wantIP, tc.wantHostPort, tc.wantGuestPort, tc.wantProto)
			}
		})
	}
}
