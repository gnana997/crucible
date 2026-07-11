package crucible

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClientWhoamiScoped(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/whoami" {
			t.Errorf("path = %s, want /whoami", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"scoped":true,"policy":{"operations":["read"],"max_sandboxes":3}}`)
	})
	wa, err := c.Whoami(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !wa.Scoped || wa.Policy == nil {
		t.Fatalf("whoami = %+v", wa)
	}
	// The policy document is opaque to the SDK — verify it round-trips
	// verbatim rather than decoding it against a daemon-side type.
	if !strings.Contains(string(wa.Policy), `"max_sandboxes":3`) {
		t.Errorf("policy payload = %s", wa.Policy)
	}
}

func TestClientWhoamiUnscoped(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"scoped":false}`)
	})
	wa, err := c.Whoami(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if wa.Scoped || wa.Policy != nil {
		t.Errorf("unscoped whoami = %+v", wa)
	}
}
