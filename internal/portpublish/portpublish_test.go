package portpublish

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// echoBackend starts a loopback TCP server that echoes each line with an
// "echo: " prefix, standing in for a guest service. Returns its
// host:port and a stop func.
func echoBackend(t *testing.T) (ip string, port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					_, _ = fmt.Fprintf(c, "echo: %s\n", sc.Text())
				}
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port, func() {
		_ = ln.Close()
		close(done)
		wg.Wait()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

func roundTrip(t *testing.T, addr, line string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(c, "%s\n", line); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimSpace(resp)
}

func TestForwarderPipesBothWays(t *testing.T) {
	gip, gport, stopBackend := echoBackend(t)
	t.Cleanup(stopBackend)

	hostPort := freePort(t)
	set, err := Publish(testLogger(), []Mapping{
		{HostIP: "127.0.0.1", HostPort: hostPort, GuestIP: gip, GuestPort: gport},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	t.Cleanup(set.Close)

	got := roundTrip(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)), "hello")
	if got != "echo: hello" {
		t.Errorf("round trip = %q, want %q", got, "echo: hello")
	}
}

func TestForwarderMultipleMappings(t *testing.T) {
	gip, gport, stop := echoBackend(t)
	t.Cleanup(stop)
	p1, p2 := freePort(t), freePort(t)
	set, err := Publish(testLogger(), []Mapping{
		{HostIP: "127.0.0.1", HostPort: p1, GuestIP: gip, GuestPort: gport},
		{HostIP: "127.0.0.1", HostPort: p2, GuestIP: gip, GuestPort: gport},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	t.Cleanup(set.Close)

	for _, p := range []int{p1, p2} {
		if got := roundTrip(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(p)), "x"); got != "echo: x" {
			t.Errorf("port %d = %q", p, got)
		}
	}
}

func TestForwarderCloseReleasesPort(t *testing.T) {
	gip, gport, stop := echoBackend(t)
	t.Cleanup(stop)
	hostPort := freePort(t)

	set, err := Publish(testLogger(), []Mapping{{HostIP: "127.0.0.1", HostPort: hostPort, GuestIP: gip, GuestPort: gport}})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	set.Close()

	// The host port must be free to re-bind after Close.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)))
	if err != nil {
		t.Fatalf("port not released after Close: %v", err)
	}
	_ = ln.Close()
}

func TestPublishBindConflictRollsBack(t *testing.T) {
	// Occupy a port, then try to publish two mappings where the second
	// collides — Publish must release the first listener it opened.
	taken := freePort(t)
	blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(taken)))
	if err != nil {
		t.Fatalf("blocker listen: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Close() })

	first := freePort(t)
	_, err = Publish(testLogger(), []Mapping{
		{HostIP: "127.0.0.1", HostPort: first, GuestIP: "127.0.0.1", GuestPort: 9},
		{HostIP: "127.0.0.1", HostPort: taken, GuestIP: "127.0.0.1", GuestPort: 9}, // conflicts
	})
	if err == nil {
		t.Fatal("Publish with a conflicting port succeeded")
	}
	// The first mapping's listener must have been closed on rollback.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(first)))
	if err != nil {
		t.Fatalf("first listener leaked after rollback: %v", err)
	}
	_ = ln.Close()
}

func TestForwarderGuestNotListening(t *testing.T) {
	// A published port whose guest backend isn't up: the client connects
	// (accept succeeds) but the forward dial fails, so the client sees a
	// promptly-closed connection — no hang.
	hostPort := freePort(t)
	deadGuest := freePort(t) // nothing listening there
	set, err := Publish(testLogger(), []Mapping{
		{HostIP: "127.0.0.1", HostPort: hostPort, GuestIP: "127.0.0.1", GuestPort: deadGuest},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	t.Cleanup(set.Close)

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)), 3*time.Second)
	if err != nil {
		t.Fatalf("dial published port: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	// Reading returns EOF (or an error) quickly once the forward dial fails.
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Error("expected the connection to close when the guest isn't listening")
	}
}

func TestCloseNilSet(t *testing.T) {
	var s *Set
	s.Close() // must not panic
}
