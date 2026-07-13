//go:build linux

package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleMountRejectsBadInput covers the validation paths that return
// before unix.Mount (which needs a real device + root). The success path is
// exercised end-to-end by scripts/smoke_volumes.sh on KVM.
func TestHandleMountRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"malformed json", `{`, 400},
		{"rootfs device rejected", `{"device":"/dev/vda","mountpoint":"/data"}`, 400},
		{"non-vd device rejected", `{"device":"/etc/passwd","mountpoint":"/data"}`, 400},
		{"relative mountpoint", `{"device":"/dev/vdb","mountpoint":"data"}`, 400},
		{"root mountpoint", `{"device":"/dev/vdb","mountpoint":"/"}`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/mount", strings.NewReader(c.body))
			w := httptest.NewRecorder()
			handleMount(w, r)
			if w.Code != c.want {
				t.Fatalf("code = %d, want %d (resp: %s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}
