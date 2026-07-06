package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gnana997/crucible/internal/api"
)

func TestClientSendsBearerToken(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(api.ListResponse{})
	}))
	defer ts.Close()

	c := New(ts.URL, WithToken("crucible_abc"))
	if _, err := c.ListSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer crucible_abc" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer crucible_abc")
	}
}

func TestClientNoTokenSendsNoHeader(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(api.ListResponse{})
	}))
	defer ts.Close()

	c := New(ts.URL)
	if _, err := c.ListSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
}
