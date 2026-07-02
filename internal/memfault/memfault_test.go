package memfault

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Firecracker-side ABI, needed only to impersonate it in tests. The
// handler never creates a userfaultfd itself — Firecracker does — so
// these constants live here, not in the package.
const (
	uffdUserModeOnly  = 0x1        // UFFD_USER_MODE_ONLY: no root needed even with vm.unprivileged_userfaultfd=0
	uffdAPI           = 0xaa       // UFFD_API version
	uffdioAPIIoctl    = 0xc018aa3f // _IOWR(0xAA, 0x3f, struct uffdio_api)
	uffdioRegIoctl    = 0xc020aa00 // _IOWR(0xAA, 0x00, struct uffdio_register)
	uffdioModeMissing = 0x1        // UFFDIO_REGISTER_MODE_MISSING
)

type uffdioAPIArg struct{ API, Features, Ioctls uint64 }

type uffdioRegisterArg struct{ Start, Len, Mode, Ioctls uint64 }

// newTestUffd creates a userfaultfd and registers region with it in
// MISSING mode, exactly what Firecracker does to the guest's RAM.
func newTestUffd(t *testing.T, region []byte) int {
	t.Helper()
	// O_NONBLOCK matters: poll(2) on a blocking userfaultfd reports
	// only POLLERR. Firecracker creates its uffd non-blocking too.
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly, 0, 0)
	if errno != 0 {
		t.Fatalf("userfaultfd: %v", errno)
	}
	api := uffdioAPIArg{API: uffdAPI}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uffdioAPIIoctl, uintptr(unsafe.Pointer(&api))); errno != 0 {
		t.Fatalf("UFFDIO_API: %v", errno)
	}
	reg := uffdioRegisterArg{
		Start: uint64(uintptr(unsafe.Pointer(&region[0]))),
		Len:   uint64(len(region)),
		Mode:  uffdioModeMissing,
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uffdioRegIoctl, uintptr(unsafe.Pointer(&reg))); errno != 0 {
		t.Fatalf("UFFDIO_REGISTER: %v", errno)
	}
	return int(fd)
}

// TestServesPagesFromMemoryFile is the full integration: a real
// userfaultfd, the real SCM_RIGHTS handshake, real faults served by
// UFFDIO_COPY. Run for both spellings of the page-size field to cover
// old and new Firecracker handshake formats.
func TestServesPagesFromMemoryFile(t *testing.T) {
	for _, field := range []string{"page_size", "page_size_kib"} {
		t.Run(field, func(t *testing.T) {
			pageSize := os.Getpagesize()
			const npages = 8

			// Backing "snapshot memory": page i filled with 'A'+i.
			content := make([]byte, npages*pageSize)
			for i := 0; i < npages; i++ {
				for j := 0; j < pageSize; j++ {
					content[i*pageSize+j] = byte('A' + i)
				}
			}
			dir := t.TempDir()
			memPath := filepath.Join(dir, "memory.file")
			if err := os.WriteFile(memPath, content, 0o644); err != nil {
				t.Fatal(err)
			}

			sock := filepath.Join(dir, "uffd.sock")
			h, err := Serve(Config{SocketPath: sock, MemPath: memPath})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()

			// --- play Firecracker ---
			region, err := unix.Mmap(-1, 0, npages*pageSize,
				unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Munmap(region)
			uffd := newTestUffd(t, region)
			defer unix.Close(uffd)

			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			layout := fmt.Sprintf(`[{"base_host_virt_addr":%d,"size":%d,"offset":0,%q:%d}]`,
				uintptr(unsafe.Pointer(&region[0])), npages*pageSize, field, pageSize)
			if _, _, err := conn.WriteMsgUnix([]byte(layout), unix.UnixRights(uffd), nil); err != nil {
				t.Fatalf("handshake send: %v", err)
			}

			// Touch pages 0, 2, 5; each read blocks until the handler
			// installs the page. Run in a goroutine so a broken handler
			// fails the test instead of hanging it.
			touched := []int{0, 2, 5}
			done := make(chan error, 1)
			go func() {
				for _, p := range touched {
					first := region[p*pageSize]
					last := region[p*pageSize+pageSize-1]
					if want := byte('A' + p); first != want || last != want {
						done <- fmt.Errorf("page %d: got %q..%q, want %q", p, first, last, want)
						return
					}
				}
				done <- nil
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("faults were never served")
			}

			// The faulting read resumes the instant UFFDIO_COPY lands,
			// slightly before the handler bumps its counter — poll
			// briefly rather than assert instantly.
			deadline := time.Now().Add(2 * time.Second)
			for h.Served() != uint64(len(touched)) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := h.Served(); got != uint64(len(touched)) {
				t.Fatalf("Served() = %d, want %d (lazy: untouched pages must not load)", got, len(touched))
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(sock); !os.IsNotExist(err) {
				t.Fatalf("socket not removed on Close: %v", err)
			}
		})
	}
}

// TestCloseBeforeConnect exercises the lifecycle where the VM never
// arrives (e.g. LoadSnapshot failed for another reason): Close must
// not hang and must be idempotent.
func TestCloseBeforeConnect(t *testing.T) {
	dir := t.TempDir()
	memPath := filepath.Join(dir, "memory.file")
	if err := os.WriteFile(memPath, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := Serve(Config{SocketPath: filepath.Join(dir, "uffd.sock"), MemPath: memPath})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		_ = h.Close()
		_ = h.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung with no connection")
	}
}

// TestCloseDuringServing verifies Close unblocks the serving loop after
// a completed handshake.
func TestCloseDuringServing(t *testing.T) {
	pageSize := os.Getpagesize()
	dir := t.TempDir()
	memPath := filepath.Join(dir, "memory.file")
	if err := os.WriteFile(memPath, make([]byte, pageSize), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "uffd.sock")
	h, err := Serve(Config{SocketPath: sock, MemPath: memPath})
	if err != nil {
		t.Fatal(err)
	}

	region, err := unix.Mmap(-1, 0, pageSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(region)
	uffd := newTestUffd(t, region)
	defer unix.Close(uffd)

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	layout := fmt.Sprintf(`[{"base_host_virt_addr":%d,"size":%d,"offset":0,"page_size":%d}]`,
		uintptr(unsafe.Pointer(&region[0])), pageSize, pageSize)
	if _, _, err := conn.WriteMsgUnix([]byte(layout), unix.UnixRights(uffd), nil); err != nil {
		t.Fatal(err)
	}

	// Serve one fault so we know the handshake finished and the loop
	// is in steady state before closing.
	if got := region[0]; got != 0 {
		t.Fatalf("page 0 = %q, want zero page", got)
	}

	closed := make(chan struct{})
	go func() {
		_ = h.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung while serving")
	}
}
