package client

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/gnana997/crucible/internal/policy"
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
	if !wa.Scoped || wa.Policy == nil || wa.Policy.MaxSandboxes != 3 || !wa.Policy.Allows(policy.OpRead) {
		t.Errorf("whoami = %+v (policy %+v)", wa, wa.Policy)
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
