package runner

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gnana997/crucible/internal/fcapi"
)

func TestValidateRestoreSpec(t *testing.T) {
	base := RestoreSpec{
		Workdir:    "/tmp/fork-x",
		StatePath:  "/snap/state.file",
		MemPath:    "/fork-x/mem.file",
		RootfsPath: "/fork-x/rootfs.ext4",
	}
	cases := []struct {
		name    string
		binary  string
		mutate  func(*RestoreSpec)
		wantErr bool
	}{
		{"valid", "firecracker", func(*RestoreSpec) {}, false},
		{"empty binary", "", func(*RestoreSpec) {}, true},
		{"empty workdir", "firecracker", func(s *RestoreSpec) { s.Workdir = "" }, true},
		{"empty state path", "firecracker", func(s *RestoreSpec) { s.StatePath = "" }, true},
		{"empty mem path", "firecracker", func(s *RestoreSpec) { s.MemPath = "" }, true},
		{"empty rootfs path", "firecracker", func(s *RestoreSpec) { s.RootfsPath = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Firecracker{Binary: tc.binary}
			s := base
			tc.mutate(&s)
			err := f.validateRestore(s)
			if tc.wantErr && err == nil {
				t.Fatalf("validateRestore(%+v): got nil, want error", s)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateRestore(%+v): got %v, want nil", s, err)
			}
		})
	}
}

func TestValidateSpec(t *testing.T) {
	// Base spec that passes all checks; each subtest perturbs one field.
	base := Spec{
		Workdir:   "/tmp/sandboxes/x",
		Kernel:    "/var/lib/crucible/vmlinux",
		Rootfs:    "/var/lib/crucible/rootfs.ext4",
		VCPUs:     2,
		MemoryMiB: 512,
	}

	tests := []struct {
		name    string
		binary  string
		mutate  func(*Spec)
		wantErr bool
	}{
		{name: "valid", binary: "firecracker", mutate: func(*Spec) {}, wantErr: false},
		{name: "empty binary", binary: "", mutate: func(*Spec) {}, wantErr: true},
		{name: "empty workdir", binary: "firecracker", mutate: func(s *Spec) { s.Workdir = "" }, wantErr: true},
		{name: "empty kernel", binary: "firecracker", mutate: func(s *Spec) { s.Kernel = "" }, wantErr: true},
		{name: "empty rootfs", binary: "firecracker", mutate: func(s *Spec) { s.Rootfs = "" }, wantErr: true},
		{name: "zero vcpus", binary: "firecracker", mutate: func(s *Spec) { s.VCPUs = 0 }, wantErr: true},
		{name: "negative memory", binary: "firecracker", mutate: func(s *Spec) { s.MemoryMiB = -1 }, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &Firecracker{Binary: tc.binary}
			s := base
			tc.mutate(&s)
			err := f.validate(s)
			if tc.wantErr && err == nil {
				t.Fatalf("validate(%+v): got nil, want error", s)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validate(%+v): got %v, want nil", s, err)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("errors.Is(err, ErrInvalidSpec) = false; err = %v", err)
			}
		})
	}
}

func TestWaitReady(t *testing.T) {
	t.Run("succeeds when API answers", func(t *testing.T) {
		sock := serveInstanceInfo(t, 0)
		client := fcapi.NewClient(sock)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := waitReady(ctx, client); err != nil {
			t.Fatalf("waitReady: %v", err)
		}
	})

	t.Run("retries until API responds", func(t *testing.T) {
		// Start the mock server after a small delay; waitReady should
		// retry through the initial connection failures and succeed.
		sockPath := filepath.Join(t.TempDir(), "fc.sock")
		go func() {
			time.Sleep(80 * time.Millisecond)
			startInstanceInfoServer(t, sockPath)
		}()
		client := fcapi.NewClient(sockPath)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := waitReady(ctx, client); err != nil {
			t.Fatalf("waitReady: %v", err)
		}
	})

	t.Run("returns deadline error if socket never appears", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "nope.sock")
		client := fcapi.NewClient(sockPath)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := waitReady(ctx, client)
		if err == nil {
			t.Fatal("waitReady: got nil, want deadline error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("waitReady err = %v, want ctx.DeadlineExceeded wrapped", err)
		}
	})
}

// serveInstanceInfo starts a mock UDS HTTP server that answers GET / with
// a minimal InstanceInfo response, and registers t.Cleanup to stop it.
// Returns the socket path.
func serveInstanceInfo(t *testing.T, delay time.Duration) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "fc.sock")
	if delay > 0 {
		time.Sleep(delay)
	}
	return startInstanceInfoServer(t, sockPath)
}

func startInstanceInfoServer(t *testing.T, sockPath string) string {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"i-1","state":"Not started","vmm_version":"1.15.1","app_name":"Firecracker"}`))
	})
	srv := &http.Server{Handler: mux, ReadTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return sockPath
}
