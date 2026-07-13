//go:build linux

package main

import "testing"

func TestCleanFreezeTarget(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/var/lib/postgresql/data", "/var/lib/postgresql/data", true},
		{"/data/", "/data", true},
		{"/data/../data", "/data", true},
		{"/", "", false},    // never freeze the rootfs
		{"", "", false},     // not absolute
		{"data", "", false}, // relative
	}
	for _, c := range cases {
		got, ok := cleanFreezeTarget(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("cleanFreezeTarget(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
