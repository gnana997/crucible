//go:build linux

package main

import (
	"net/http/httptest"
	"testing"
)

// withSyncer swaps the sync(2) seam for a stub, counting invocations.
func withSyncer(t *testing.T, hook func()) *int {
	t.Helper()
	var calls int
	orig := syncer
	syncer = func() {
		calls++
		hook()
	}
	t.Cleanup(func() { syncer = orig })
	return &calls
}

func TestQuiesceSyncsAndReturnsOK(t *testing.T) {
	calls := withSyncer(t, func() {})

	rec := httptest.NewRecorder()
	handleQuiesce(rec, httptest.NewRequest("POST", "/quiesce", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	if *calls != 1 {
		t.Fatalf("sync called %d times, want 1", *calls)
	}
}
