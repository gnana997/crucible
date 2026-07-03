package network

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// TestRanAndExitedNonZeroExcludesDeadlineKill is the N5 guard: a command
// SIGKILLed by its own context deadline returns "signal: killed" as an
// *exec.ExitError — which errors.As accepts — so without the ctx.Err() check
// a timed-out teardown would be misclassified as "already gone" and leak the
// object. The guard must return false whenever the ctx is done.
func TestRanAndExitedNonZeroExcludesDeadlineKill(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := runCmd(ctx, "sleep", "5")
	if err == nil {
		t.Fatal("expected the deadline to kill sleep, got nil error")
	}
	// The hazard: the error genuinely wraps an *exec.ExitError.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an *exec.ExitError from the killed command, got %v", err)
	}
	// The fix: the ctx being done makes the guard reject it as fatal.
	if ranAndExitedNonZero(ctx, err) {
		t.Error("ranAndExitedNonZero returned true for a deadline-killed command — would be misread as 'already gone'")
	}
}

// TestRanAndExitedNonZeroTrueOnGenuineExit confirms the guard still accepts a
// real non-zero exit when the ctx is live — the case idempotent teardown must
// keep recognizing.
func TestRanAndExitedNonZeroTrueOnGenuineExit(t *testing.T) {
	if _, err := exec.LookPath("false"); err != nil {
		t.Skipf("false unavailable: %v", err)
	}
	ctx := context.Background()
	err := runCmd(ctx, "false")
	if err == nil {
		t.Fatal("expected non-zero exit from `false`")
	}
	if !ranAndExitedNonZero(ctx, err) {
		t.Error("ranAndExitedNonZero returned false for a genuine non-zero exit under a live ctx")
	}
}

// TestIsNoSuchObjectRespectsCtx checks the consumer end: the same
// "does not exist" error is idempotent-success under a live ctx but fatal once
// the ctx is cancelled, so a cancelled/timed-out nft call can never be bucketed
// as "already gone".
func TestIsNoSuchObjectRespectsCtx(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	live := context.Background()
	// A real *exec.ExitError whose captured stderr carries the phrase.
	err := runCmd(live, "sh", "-c", "echo 'does not exist' >&2; exit 1")
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !isNoSuchObject(live, err) {
		t.Fatalf("isNoSuchObject = false under live ctx, want true (err=%v)", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if isNoSuchObject(cancelled, err) {
		t.Error("isNoSuchObject = true under a cancelled ctx — a cancelled teardown would leak the object as 'already gone'")
	}
}
