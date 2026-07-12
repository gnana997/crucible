//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gnana997/crucible/internal/agentwire"
)

// withClockStepper swaps the settimeofday(2) seam for a stub, recording the
// wall times it was handed.
func withClockStepper(t *testing.T, hook func(unixNano int64) error) *[]int64 {
	t.Helper()
	var times []int64
	orig := clockStepper
	clockStepper = func(unixNano int64) error {
		times = append(times, unixNano)
		return hook(unixNano)
	}
	t.Cleanup(func() { clockStepper = orig })
	return &times
}

func wakeBody(t *testing.T, seed []byte, wallTimeUnixNano int64) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(agentwire.WakeRequest{Seed: seed, WallTimeUnixNano: wallTimeUnixNano})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}

func TestWakeHappy(t *testing.T) {
	seeds := withEntropyInjector(t, func([]byte) error { return nil })
	times := withClockStepper(t, func(int64) error { return nil })

	seed := bytes.Repeat([]byte{0xab}, identitySeedSize)
	const wall = int64(1_700_000_000_000_000_000)
	rec := httptest.NewRecorder()
	handleWake(rec, httptest.NewRequest("POST", "/wake", wakeBody(t, seed, wall)))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	// Reseeds the CRNG with exactly the host seed...
	if len(*seeds) != 1 || !bytes.Equal((*seeds)[0], seed) {
		t.Fatalf("entropy seeds = %v, want one call with the request seed", *seeds)
	}
	// ...and steps the clock to the host wall time.
	if len(*times) != 1 || (*times)[0] != wall {
		t.Fatalf("clock steps = %v, want [%d]", *times, wall)
	}
}

// Wake must NOT touch identity: the hostname setter is never invoked.
func TestWakePreservesIdentity(t *testing.T) {
	withEntropyInjector(t, func([]byte) error { return nil })
	withClockStepper(t, func(int64) error { return nil })
	names := withHostnameSetter(t, func([]byte) error { return nil })

	rec := httptest.NewRecorder()
	handleWake(rec, httptest.NewRequest("POST", "/wake",
		wakeBody(t, bytes.Repeat([]byte{1}, identitySeedSize), 1_700_000_000_000_000_000)))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(*names) != 0 {
		t.Fatalf("wake rotated the hostname (%v); it must preserve identity", *names)
	}
}

func TestWakeRejectsBadRequest(t *testing.T) {
	withEntropyInjector(t, func([]byte) error { return nil })
	withClockStepper(t, func(int64) error { return nil })

	cases := []struct {
		name string
		seed []byte
		wall int64
	}{
		{"short seed", bytes.Repeat([]byte{1}, 16), 1_700_000_000_000_000_000},
		{"missing wall time", bytes.Repeat([]byte{1}, identitySeedSize), 0},
		{"negative wall time", bytes.Repeat([]byte{1}, identitySeedSize), -5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleWake(rec, httptest.NewRequest("POST", "/wake", wakeBody(t, tc.seed, tc.wall)))
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body)
			}
		})
	}
}

func TestWakeFatalOnStepFailures(t *testing.T) {
	seed := bytes.Repeat([]byte{1}, identitySeedSize)
	const wall = int64(1_700_000_000_000_000_000)

	t.Run("entropy failure is 500", func(t *testing.T) {
		withEntropyInjector(t, func([]byte) error { return errors.New("boom") })
		withClockStepper(t, func(int64) error { return nil })
		rec := httptest.NewRecorder()
		handleWake(rec, httptest.NewRequest("POST", "/wake", wakeBody(t, seed, wall)))
		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("clock failure is 500", func(t *testing.T) {
		withEntropyInjector(t, func([]byte) error { return nil })
		withClockStepper(t, func(int64) error { return errors.New("boom") })
		rec := httptest.NewRecorder()
		handleWake(rec, httptest.NewRequest("POST", "/wake", wakeBody(t, seed, wall)))
		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}
