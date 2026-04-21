//go:build linux

package main

import (
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/agentwire"
)

func TestTimevalToMs(t *testing.T) {
	cases := []struct {
		name string
		tv   syscall.Timeval
		want int64
	}{
		{"zero", syscall.Timeval{Sec: 0, Usec: 0}, 0},
		{"1ms exact", syscall.Timeval{Sec: 0, Usec: 1000}, 1},
		{"500us rounds down", syscall.Timeval{Sec: 0, Usec: 500}, 0},
		{"1.234s", syscall.Timeval{Sec: 1, Usec: 234000}, 1234},
		{"12.5s", syscall.Timeval{Sec: 12, Usec: 500000}, 12500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := timevalToMs(tc.tv); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBuildUsageRusageMapping(t *testing.T) {
	ru := &syscall.Rusage{
		Utime:  syscall.Timeval{Sec: 1, Usec: 250000}, // 1250 ms
		Stime:  syscall.Timeval{Sec: 0, Usec: 100000}, // 100 ms
		Maxrss: 2048,                                  // 2 MiB in KiB
		Majflt: 7,
		Nivcsw: 42,
	}
	io := &procIOStats{ReadBytes: 100, WriteBytes: 200}
	u := buildUsage(ru, io)
	if u == nil {
		t.Fatal("buildUsage returned nil")
	}
	if u.CPUUserMs != 1250 || u.CPUSysMs != 100 {
		t.Errorf("CPU ms: user=%d sys=%d, want 1250/100", u.CPUUserMs, u.CPUSysMs)
	}
	if u.PeakMemoryBytes != 2048*1024 {
		t.Errorf("PeakMemoryBytes = %d, want %d (Maxrss KiB → bytes)", u.PeakMemoryBytes, 2048*1024)
	}
	if u.PageFaultsMajor != 7 {
		t.Errorf("PageFaultsMajor = %d, want 7", u.PageFaultsMajor)
	}
	if u.ContextSwitchesInvoluntary != 42 {
		t.Errorf("Nivcsw = %d, want 42", u.ContextSwitchesInvoluntary)
	}
	if u.IOReadBytes != 100 || u.IOWriteBytes != 200 {
		t.Errorf("IO: r=%d w=%d, want 100/200", u.IOReadBytes, u.IOWriteBytes)
	}
}

func TestBuildUsageNilIO(t *testing.T) {
	ru := &syscall.Rusage{Utime: syscall.Timeval{Sec: 0, Usec: 1000}}
	u := buildUsage(ru, nil)
	if u == nil {
		t.Fatal("buildUsage(nil io) returned nil struct")
	}
	if u.IOReadBytes != 0 || u.IOWriteBytes != 0 {
		t.Errorf("IO on nil ioStats: r=%d w=%d, want 0/0", u.IOReadBytes, u.IOWriteBytes)
	}
}

func TestReadProcIOOnSelf(t *testing.T) {
	// /proc/self/io must exist on any Linux kernel we run on, and the
	// counters are non-negative. Exercises the parse path end-to-end.
	stats, err := readProcIO(os.Getpid())
	if err != nil {
		t.Fatalf("readProcIO(self): %v", err)
	}
	if stats.ReadBytes < 0 || stats.WriteBytes < 0 {
		t.Errorf("negative counters: %+v", stats)
	}
}

func TestReadProcIOMissingPidReturnsNotExist(t *testing.T) {
	// PID 0x7fffffff is effectively guaranteed not to exist — the
	// kernel's default pid_max is 2^22, and even with pid_max raised
	// it's unheard of.
	_, err := readProcIO(0x7fffffff)
	if err == nil {
		t.Fatal("expected error for nonexistent pid")
	}
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want os.IsNotExist-compatible", err)
	}
}

func TestPollIOExitsOnStop(t *testing.T) {
	// Smoke: the poller must exit promptly when stop closes, even
	// while ticking.
	var last atomic.Pointer[procIOStats]
	stop := make(chan struct{})
	done := make(chan struct{})
	go pollIO(os.Getpid(), &last, stop, done)

	// Let it do at least one read (guaranteed by the initial read
	// before the ticker loop in pollIO).
	time.Sleep(10 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("pollIO did not exit within 1s of stop")
	}
	if last.Load() == nil {
		t.Error("pollIO captured no reads at all")
	}
}

func TestGuestMemTotalBytesNonZero(t *testing.T) {
	// On any running Linux, MemTotal is > 0. We don't assert a
	// specific value because it depends on the host/VM.
	got := guestMemTotalBytes()
	if got <= 0 {
		t.Fatalf("guestMemTotalBytes = %d, want > 0", got)
	}
}

// processStateForSignal runs `sh -c '<payload>'` and returns its
// ProcessState. Used by OOM-detection tests because os.ProcessState
// is not directly constructable — it has to come from a real child.
func processStateForSignal(t *testing.T, payload string) *os.ProcessState {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", payload)
	_ = cmd.Run() // error expected for signal payloads; we want ProcessState regardless
	if cmd.ProcessState == nil {
		t.Fatalf("no ProcessState after running %q", payload)
	}
	return cmd.ProcessState
}

func TestDetectOOMSIGKILLAboveThreshold(t *testing.T) {
	ps := processStateForSignal(t, "kill -9 $$")
	// peak == guest total → exactly 100%, definitely >= 95%.
	if !detectOOM(ps, false, 256<<20, 256<<20) {
		t.Error("expected OomKilled=true for SIGKILL with peak==total")
	}
}

func TestDetectOOMSIGKILLBelowThreshold(t *testing.T) {
	ps := processStateForSignal(t, "kill -9 $$")
	// 50% usage — well below 95% threshold, not an OOM.
	if detectOOM(ps, false, 128<<20, 256<<20) {
		t.Error("expected OomKilled=false when peak well below threshold")
	}
}

func TestDetectOOMTimedOutIsNotOOM(t *testing.T) {
	// Even with a SIGKILL and high memory, a user-requested timeout
	// (TimedOut=true) must not register as OOM — we killed it, not
	// the kernel.
	ps := processStateForSignal(t, "kill -9 $$")
	if detectOOM(ps, true, 256<<20, 256<<20) {
		t.Error("expected OomKilled=false when TimedOut")
	}
}

func TestDetectOOMCleanExitIsNotOOM(t *testing.T) {
	ps := processStateForSignal(t, "exit 0")
	if detectOOM(ps, false, 256<<20, 256<<20) {
		t.Error("expected OomKilled=false on clean exit")
	}
}

func TestDetectOOMNonZeroExitIsNotOOM(t *testing.T) {
	ps := processStateForSignal(t, "exit 1")
	if detectOOM(ps, false, 256<<20, 256<<20) {
		t.Error("expected OomKilled=false on non-zero exit (not a signal)")
	}
}

func TestDetectOOMUnknownGuestMemoryDoesNotFire(t *testing.T) {
	ps := processStateForSignal(t, "kill -9 $$")
	if detectOOM(ps, false, 256<<20, 0) {
		t.Error("expected OomKilled=false when guestMemBytes is unknown (0)")
	}
}

func TestAttachUsageRealCommand(t *testing.T) {
	// Run a tiny command end-to-end and verify attachUsage populates
	// Usage with sensible values. Doesn't exercise the I/O poller —
	// TestPollIOExitsOnStop and TestReadProcIOOnSelf cover that path
	// separately.
	cmd := exec.Command("/bin/sh", "-c", "echo hello >/dev/null")
	if err := cmd.Run(); err != nil {
		t.Fatalf("cmd.Run: %v", err)
	}

	var res agentwire.ExecResult
	attachUsage(&res, cmd.ProcessState, nil)

	if res.Usage == nil {
		t.Fatal("Usage was not attached after a successful Run")
	}
	if res.Usage.PeakMemoryBytes <= 0 {
		t.Errorf("PeakMemoryBytes = %d, want > 0", res.Usage.PeakMemoryBytes)
	}
	if res.Usage.CPUUserMs+res.Usage.CPUSysMs < 0 {
		t.Errorf("negative CPU totals: %+v", res.Usage)
	}
	if res.OomKilled {
		t.Error("OomKilled should be false for clean exit")
	}
}

func TestAttachUsageNilProcessStateIsNoop(t *testing.T) {
	// Parity with the start-failure path in handleExec: attachUsage on
	// a nil ProcessState must leave the result untouched rather than
	// panicking.
	var res agentwire.ExecResult
	attachUsage(&res, nil, nil)
	if res.Usage != nil {
		t.Error("Usage unexpectedly populated from nil ProcessState")
	}
	if res.OomKilled {
		t.Error("OomKilled unexpectedly set from nil ProcessState")
	}
}
