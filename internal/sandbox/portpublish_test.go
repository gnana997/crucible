package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakePublisher records Publish calls and hands back a handle that
// flips a flag on Close.
type fakePublisher struct {
	mu        sync.Mutex
	publishes []publishCall
	err       error
}

type publishCall struct {
	sandboxID string
	guestIP   string
	ports     []PortMapping
	handle    *fakeHandle
}

type fakeHandle struct{ closed bool }

func (h *fakeHandle) Close() { h.closed = true }

func (p *fakePublisher) Publish(_ context.Context, sandboxID, guestIP string, ports []PortMapping) (PublishHandle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	h := &fakeHandle{}
	p.publishes = append(p.publishes, publishCall{sandboxID, guestIP, ports, h})
	return h, nil
}

func (p *fakePublisher) last() publishCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishes[len(p.publishes)-1]
}

func newPublishManager(t *testing.T, pub PortPublisher) *Manager {
	t.Helper()
	tmpl := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(tmpl, []byte("fake"), 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(ManagerConfig{
		Runner:        &agentRunner{t: t, handler: (&netConfigRecorder{}).handler()},
		WorkBase:      t.TempDir(),
		Kernel:        "/fake/vmlinux",
		Rootfs:        tmpl,
		WaitForAgent:  true,
		Network:       staticNetProvisioner{},
		PortPublisher: pub,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Shutdown(context.Background()) })
	return m
}

func TestCreatePublishesPorts(t *testing.T) {
	pub := &fakePublisher{}
	m := newPublishManager(t, pub)

	sb, err := m.Create(context.Background(), CreateConfig{
		Network: &NetworkConfig{Allowlist: stubAllowlist{}},
		Publish: []PortMapping{{HostPort: 8080, GuestPort: 80, Protocol: "tcp"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	call := pub.last()
	if call.sandboxID != sb.ID {
		t.Errorf("published for %q, want %q", call.sandboxID, sb.ID)
	}
	// staticNetProvisioner hands out 10.20.0.6.
	if call.guestIP != "10.20.0.6" {
		t.Errorf("published to guest IP %q, want 10.20.0.6", call.guestIP)
	}
	if len(call.ports) != 1 || call.ports[0].HostPort != 8080 || call.ports[0].GuestPort != 80 {
		t.Errorf("published ports = %+v", call.ports)
	}
	if len(sb.Published) != 1 {
		t.Errorf("sandbox.Published = %+v", sb.Published)
	}

	// Delete closes the forwarders.
	if err := m.Delete(context.Background(), sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !call.handle.closed {
		t.Error("port forwarders not closed on Delete")
	}
}

func TestCreatePublishWithoutNetworkRejected(t *testing.T) {
	pub := &fakePublisher{}
	m := newPublishManager(t, pub)

	// Publish but no Network → no guest IP to forward to.
	_, err := m.Create(context.Background(), CreateConfig{
		Publish: []PortMapping{{HostPort: 8080, GuestPort: 80}},
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if len(pub.publishes) != 0 {
		t.Error("publisher was called despite the rejection")
	}
}

func TestCreatePublishNoPublisherRejected(t *testing.T) {
	m := newPublishManager(t, nil) // PortPublisher not configured
	_, err := m.Create(context.Background(), CreateConfig{
		Network: &NetworkConfig{Allowlist: stubAllowlist{}},
		Publish: []PortMapping{{HostPort: 8080, GuestPort: 80}},
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestCreatePublishFailureRollsBack(t *testing.T) {
	pub := &fakePublisher{err: errors.New("bind: address already in use")}
	m := newPublishManager(t, pub)

	_, err := m.Create(context.Background(), CreateConfig{
		Network: &NetworkConfig{Allowlist: stubAllowlist{}},
		Publish: []PortMapping{{HostPort: 8080, GuestPort: 80}},
	})
	if err == nil {
		t.Fatal("Create succeeded despite a publish bind failure")
	}
	// The failed create must leave no live sandbox.
	if got := len(m.List()); got != 0 {
		t.Errorf("List has %d sandboxes after a rolled-back create, want 0", got)
	}
}
