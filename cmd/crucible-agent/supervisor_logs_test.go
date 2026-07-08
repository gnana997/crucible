//go:build linux

package main

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

func TestSupervisorLogsBeforeConfigure(t *testing.T) {
	s := newTestSupervisor(t, &fakeRunner{}, newFakeClock())
	if _, err := s.Logs(0, 1<<20); !errors.Is(err, errNoServiceSpec) {
		t.Fatalf("Logs err = %v, want errNoServiceSpec", err)
	}
}

func TestSupervisorLogsCaptureAndStatusCursors(t *testing.T) {
	fr := &fakeRunner{}
	fr.enqueue(newFakeChild(101))
	s := newTestSupervisor(t, fr, newFakeClock())

	mustConfigureStart(t, s, specWith("/bin/app"))
	stdout, stderr := fr.writers()
	_, _ = stdout.Write([]byte("serving on :8080\n"))
	_, _ = stderr.Write([]byte("warn: something\n"))

	resp, err := s.Logs(0, 1<<20)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(resp.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(resp.Records))
	}
	if resp.Records[0].Stream != agentwire.ServiceLogStdout {
		t.Errorf("rec0 stream = %q", resp.Records[0].Stream)
	}
	if resp.Records[1].Stream != agentwire.ServiceLogStderr {
		t.Errorf("rec1 stream = %q", resp.Records[1].Stream)
	}

	st, err := s.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.LogNextSeq != 2 || st.LogFirstSeq != 0 {
		t.Errorf("status cursors = first %d next %d, want 0,2", st.LogFirstSeq, st.LogNextSeq)
	}
}

func TestSupervisorLogsSurviveRestartResetOnRespec(t *testing.T) {
	fr := &fakeRunner{}
	fc1, fc2, fc3 := newFakeChild(101), newFakeChild(102), newFakeChild(103)
	fc1.exitOn(syscall.SIGTERM, childExit{})
	fc2.exitOn(syscall.SIGTERM, childExit{})
	fr.enqueue(fc1, fc2, fc3)
	s := newTestSupervisor(t, fr, newFakeClock())

	mustConfigureStart(t, s, specWith("/bin/app", "v1"))
	stdout, _ := fr.writers()
	_, _ = stdout.Write([]byte("from run 1\n"))

	// Restart under the same spec: history survives.
	if _, err := s.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	waitForState(t, s, agentwire.ServiceStateRunning)
	resp, err := s.Logs(0, 1<<20)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(resp.Records) != 1 || string(resp.Records[0].Data) != "from run 1\n" {
		t.Fatalf("history lost across same-spec restart: %+v", resp.Records)
	}

	// Re-spec: fresh ring.
	if _, err := s.Configure(specWith("/bin/app", "v2")); err != nil {
		t.Fatalf("re-Configure: %v", err)
	}
	waitForState(t, s, agentwire.ServiceStateRunning)
	resp, err = s.Logs(0, 1<<20)
	if err != nil {
		t.Fatalf("Logs after respec: %v", err)
	}
	if len(resp.Records) != 0 || resp.NextSeq != 0 {
		t.Fatalf("ring not reset on respec: %+v", resp)
	}
}

func TestServiceRealLogsCaptured(t *testing.T) {
	s := newRealSupervisor(t)
	mustConfigureStart(t, s, specWith("/bin/sh", "-c", `echo out-line; echo err-line 1>&2; sleep 60`))

	deadline := time.Now().Add(5 * time.Second)
	var got map[string]string
	for {
		resp, err := s.Logs(0, 1<<20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		got = map[string]string{}
		for _, rec := range resp.Records {
			got[rec.Stream] += string(rec.Data)
		}
		if got[agentwire.ServiceLogStdout] == "out-line\n" && got[agentwire.ServiceLogStderr] == "err-line\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured logs = %+v, want both lines", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.Stop(time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForState(t, s, agentwire.ServiceStateStopped)
}
