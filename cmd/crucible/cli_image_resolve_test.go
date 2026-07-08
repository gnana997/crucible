package main

import (
	"context"
	"io"
	"testing"
)

// The docker-save/import branch needs a real Docker + daemon (the smoke
// covers it); here we pin the two pure-logic branches that must never
// touch Docker or the client — so a nil client is a deliberate tripwire.

func TestResolveCreateImageAlwaysSkipsDocker(t *testing.T) {
	// --pull always forces a registry pull: pass the ref through
	// unchanged and never probe/import via Docker.
	ref, pull, err := resolveCreateImage(context.Background(), nil, "myapp:local", "always", io.Discard)
	if err != nil {
		t.Fatalf("resolveCreateImage: %v", err)
	}
	if ref != "myapp:local" || pull != "always" {
		t.Errorf("got (%q, %q), want (myapp:local, always)", ref, pull)
	}
}

func TestResolveCreateImagePassesThroughUnknownLocal(t *testing.T) {
	// A ref the local Docker daemon doesn't have (a deliberately absurd
	// name) is passed through for the daemon to resolve from its store or
	// a registry — with the pull policy intact.
	const ref = "no-such-local-image-8f3a2b1c:zzz"
	got, pull, err := resolveCreateImage(context.Background(), nil, ref, "missing", io.Discard)
	if err != nil {
		t.Fatalf("resolveCreateImage: %v", err)
	}
	if got != ref || pull != "missing" {
		t.Errorf("got (%q, %q), want (%q, missing)", got, pull, ref)
	}
}
