package fcapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// newMockServer starts an HTTP server on a temporary unix socket and
// returns the socket path. The server is stopped automatically via
// t.Cleanup. Tests can install handlers by wrapping http.ServeMux.
func newMockServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "fc.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{
		Handler:     handler,
		ReadTimeout: 5 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return sockPath
}

// assertBody decodes req into v and fails the test if it can't be parsed.
func assertBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if got, want := r.Header.Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(buf, v); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, buf)
	}
}

func TestGetInstanceInfo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "i-1",
			"state": "Not started",
			"vmm_version": "1.15.1",
			"app_name": "Firecracker"
		}`))
	})
	sock := newMockServer(t, mux)

	c := NewClient(sock)
	info, err := c.GetInstanceInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInstanceInfo: %v", err)
	}
	if info.State != StateNotStarted {
		t.Errorf("State = %q, want %q", info.State, StateNotStarted)
	}
	if info.VMMVersion != "1.15.1" {
		t.Errorf("VMMVersion = %q, want 1.15.1", info.VMMVersion)
	}
}

func TestPutBootSource(t *testing.T) {
	var got BootSource
	mux := http.NewServeMux()
	mux.HandleFunc("/boot-source", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		assertBody(t, r, &got)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newMockServer(t, mux)

	c := NewClient(sock)
	want := BootSource{
		KernelImagePath: "/tmp/vmlinux",
		BootArgs:        "console=ttyS0 reboot=k",
	}
	if err := c.PutBootSource(context.Background(), want); err != nil {
		t.Fatalf("PutBootSource: %v", err)
	}
	if got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
}

func TestPutDrive(t *testing.T) {
	var got Drive
	mux := http.NewServeMux()
	mux.HandleFunc("/drives/rootfs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		assertBody(t, r, &got)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newMockServer(t, mux)

	c := NewClient(sock)
	want := Drive{
		DriveID:      "rootfs",
		PathOnHost:   "/tmp/rootfs.ext4",
		IsRootDevice: true,
	}
	if err := c.PutDrive(context.Background(), want); err != nil {
		t.Fatalf("PutDrive: %v", err)
	}
	if got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
}

func TestPutMachineConfig(t *testing.T) {
	var got MachineConfig
	mux := http.NewServeMux()
	mux.HandleFunc("/machine-config", func(w http.ResponseWriter, r *http.Request) {
		assertBody(t, r, &got)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newMockServer(t, mux)

	c := NewClient(sock)
	want := MachineConfig{VCPUCount: 2, MemSizeMiB: 512}
	if err := c.PutMachineConfig(context.Background(), want); err != nil {
		t.Fatalf("PutMachineConfig: %v", err)
	}
	if got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
}

func TestInstanceStart(t *testing.T) {
	var got action
	mux := http.NewServeMux()
	mux.HandleFunc("/actions", func(w http.ResponseWriter, r *http.Request) {
		assertBody(t, r, &got)
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newMockServer(t, mux)

	c := NewClient(sock)
	if err := c.InstanceStart(context.Background()); err != nil {
		t.Fatalf("InstanceStart: %v", err)
	}
	if got.ActionType != ActionInstanceStart {
		t.Errorf("action_type = %q, want %q", got.ActionType, ActionInstanceStart)
	}
}

func TestErrorPathFaultMessage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/boot-source", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"fault_message": "kernel_image_path is required"}`))
	})
	sock := newMockServer(t, mux)

	c := NewClient(sock)
	err := c.PutBootSource(context.Background(), BootSource{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is %T, want *Error", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want 400", apiErr.Status)
	}
	if apiErr.FaultMessage != "kernel_image_path is required" {
		t.Errorf("FaultMessage = %q, want kernel_image_path is required", apiErr.FaultMessage)
	}
	if !IsStatus(err, http.StatusBadRequest) {
		t.Error("IsStatus(err, 400) = false, want true")
	}
	if IsStatus(err, http.StatusNotFound) {
		t.Error("IsStatus(err, 404) = true, want false")
	}
}

func TestErrorPathNonJSONBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<html>oops</html>`))
	})
	sock := newMockServer(t, mux)

	c := NewClient(sock)
	_, err := c.GetInstanceInfo(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if apiErr.FaultMessage != "" {
		t.Errorf("FaultMessage = %q, want empty (body was not JSON)", apiErr.FaultMessage)
	}
	if apiErr.RawBody != "<html>oops</html>" {
		t.Errorf("RawBody = %q, want <html>oops</html>", apiErr.RawBody)
	}
}

func TestContextCancellation(t *testing.T) {
	// Handler sleeps long enough that the client context deadline fires first.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	sock := newMockServer(t, mux)

	c := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.GetInstanceInfo(ctx)
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}
