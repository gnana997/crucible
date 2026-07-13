package capture

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestValidFilter(t *testing.T) {
	ok := []string{"", "tcp port 80", "host 10.0.0.5 and udp", "portrange 8000-9000", "not arp", "tcp[13] & 2 != 0"}
	bad := []string{"-r /etc/passwd", "; rm -rf /", "tcp && $(x)", "`id`", "-i eth0", "\ttab"}
	for _, f := range ok {
		if !ValidFilter(f) {
			t.Errorf("ValidFilter(%q) = false, want true", f)
		}
	}
	for _, f := range bad {
		if ValidFilter(f) {
			t.Errorf("ValidFilter(%q) = true, want false (must reject)", f)
		}
	}
}

func TestTcpdumpArgsSingleFilterArg(t *testing.T) {
	a := tcpdumpArgs(Options{Iface: "vh-abc", Filter: "tcp port 80", Snaplen: 128})
	joined := strings.Join(a, " ")
	if !strings.Contains(joined, "-i vh-abc") || !strings.Contains(joined, "-w -") || !strings.Contains(joined, "-s 128") {
		t.Errorf("args = %v", a)
	}
	// The filter MUST be one trailing argument — never split — so it can't inject
	// a tcpdump option.
	if a[len(a)-1] != "tcp port 80" {
		t.Errorf("filter not a single trailing arg: %v", a)
	}
}

func TestNormalizedDefaults(t *testing.T) {
	n := Options{}.Normalized()
	if n.MaxBytes != DefaultMaxBytes || n.MaxDur != DefaultMaxDur || n.Snaplen != DefaultSnaplen {
		t.Errorf("defaults not applied: %+v", n)
	}
	// explicit values are preserved
	n2 := Options{Snaplen: 9, MaxBytes: 5, MaxDur: time.Second}.Normalized()
	if n2.Snaplen != 9 || n2.MaxBytes != 5 || n2.MaxDur != time.Second {
		t.Errorf("explicit values overwritten: %+v", n2)
	}
}

func TestRunValidates(t *testing.T) {
	if err := Run(context.Background(), Options{}, io.Discard); err == nil {
		t.Error("expected error for empty iface")
	}
	if err := Run(context.Background(), Options{Iface: "x", Filter: "-r /etc/passwd"}, io.Discard); err == nil {
		t.Error("expected error for a bad filter")
	}
}
