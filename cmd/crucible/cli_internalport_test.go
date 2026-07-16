package main

import "testing"

func TestParseInternalPort(t *testing.T) {
	cases := []struct {
		spec      string
		wantPort  int
		wantProto string
		wantErr   bool
	}{
		{spec: "5432", wantPort: 5432, wantProto: "tcp"},        // default proto
		{spec: "5432/tcp", wantPort: 5432, wantProto: "tcp"},    //
		{spec: "80/http", wantPort: 80, wantProto: "http"},      //
		{spec: " 6379 /TCP ", wantPort: 6379, wantProto: "tcp"}, // trimmed + case-insensitive
		{spec: "0", wantErr: true},                              // out of range
		{spec: "70000", wantErr: true},                          // out of range
		{spec: "5432/udp", wantErr: true},                       // unknown proto
		{spec: "notaport", wantErr: true},                       //
		{spec: "", wantErr: true},                               //
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			ip, err := parseInternalPort(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseInternalPort(%q) = %+v, want error", tc.spec, ip)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseInternalPort(%q): %v", tc.spec, err)
			}
			if ip.Port != tc.wantPort || ip.Proto != tc.wantProto {
				t.Fatalf("parseInternalPort(%q) = {%d,%q}, want {%d,%q}", tc.spec, ip.Port, ip.Proto, tc.wantPort, tc.wantProto)
			}
		})
	}
}
