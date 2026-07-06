// Package memfault serves guest memory page faults for Firecracker's
// userfaultfd ("Uffd") snapshot-load backend.
//
// One Handler runs per restored VM:
//
//  1. The runner creates a Handler listening on a Unix socket before
//     issuing PUT /snapshot/load with backend_type=Uffd and
//     backend_path pointing at that socket.
//  2. During load, Firecracker connects, sends a JSON array describing
//     the guest memory layout, and passes the userfaultfd it created
//     over the socket via SCM_RIGHTS.
//  3. The Handler answers every missing-page fault by reading the
//     faulting page from the snapshot's memory file and installing it
//     with UFFDIO_COPY, which atomically resumes the faulting vCPU.
//
// UFFDIO_COPY lands pages in the restoring VM's private anonymous
// memory, so any number of forks can share one snapshot memory file
// read-only and still diverge on write. Pages the guest never touches
// are never read — fork cost is O(guest working set) regardless of
// filesystem reflink support.
package memfault

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Values from linux/userfaultfd.h. x/sys/unix carries only the syscall
// number, so the ioctl ABI is declared by hand (layout verified against
// the C header; struct uffd_msg is packed, 32 bytes, fault address at
// byte offset 16).
const (
	uffdioCopyIoctl    = 0xc028aa03 // _IOWR(0xAA, 0x03, struct uffdio_copy)
	uffdEventPagefault = 0x12       // UFFD_EVENT_PAGEFAULT
	uffdMsgSize        = 32         // sizeof(struct uffd_msg)
	uffdMsgAddrOffset  = 16         // offsetof(uffd_msg, arg.pagefault.address)

	defaultPageSize = 4096
)

// uffdioCopy mirrors struct uffdio_copy: five naturally-aligned 8-byte
// fields, 40 bytes total.
type uffdioCopy struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// guestRegionUffdMapping mirrors the JSON objects Firecracker sends in
// its uffd handshake. Firecracker v1.8+ calls the last field page_size;
// older releases called it page_size_kib (the value was always bytes,
// despite the name). Accept both.
type guestRegionUffdMapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
	PageSizeKib      uint64 `json:"page_size_kib"`
}

func (m guestRegionUffdMapping) pageSize() uint64 {
	if m.PageSize != 0 {
		return m.PageSize
	}
	if m.PageSizeKib != 0 {
		return m.PageSizeKib
	}
	return defaultPageSize
}

// Config declares one Handler.
type Config struct {
	// SocketPath is the Unix socket the Handler listens on and
	// Firecracker connects to. Pass the same path (chroot-relative
	// under jailer) as LoadSnapshot's backend_path.
	SocketPath string

	// MemPath is the snapshot memory file pages are served from.
	// Opened read-only and held open for the Handler's lifetime, so
	// the snapshot may be unlinked while restored VMs still run.
	MemPath string

	// SocketUID/SocketGID, when non-zero, own the socket inode: under
	// jailer firecracker connects as an unprivileged uid, so the socket is
	// chowned to it and made owner-only (0600) rather than world-writable.
	// Zero (direct-exec/dev/test) leaves the socket owned by the crucible
	// process — which is also the uid firecracker runs as there — and
	// still owner-only. This closes the window where any local process
	// could connect first and feed a bogus layout/fd.
	SocketUID uint32
	SocketGID uint32

	// Logger receives lifecycle events. Nil means slog.Default.
	Logger *slog.Logger
}

// Handler owns the socket, the received userfaultfd, and the fault-
// serving goroutine for a single restored VM.
type Handler struct {
	ln   *net.UnixListener
	memF *os.File
	sock string
	log  *slog.Logger

	// wakeW is closed by Close to make wakeR readable, which pops the
	// serving goroutine out of poll(2) at any point in its lifecycle.
	wakeR *os.File
	wakeW *os.File

	done      chan struct{} // closed when the serving goroutine exits
	closeOnce sync.Once
	served    atomic.Uint64
}

var errClosed = errors.New("memfault: handler closed")

// Serve binds SocketPath and starts the serving goroutine. It returns
// as soon as the socket is listening — callers may immediately hand the
// path to Firecracker. Always Close the Handler, even on later errors:
// the goroutine and two fds live until then.
func Serve(cfg Config) (*Handler, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	memF, err := os.Open(cfg.MemPath)
	if err != nil {
		return nil, fmt.Errorf("memfault: open memory file: %w", err)
	}
	// A stale socket from a crashed previous run would fail the bind.
	_ = os.Remove(cfg.SocketPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.SocketPath, Net: "unix"})
	if err != nil {
		_ = memF.Close()
		return nil, fmt.Errorf("memfault: listen on %s: %w", cfg.SocketPath, err)
	}
	// Restrict the socket to the single uid firecracker runs as instead of
	// leaving it world-writable. Under jailer that's an unprivileged jail
	// uid, so hand it ownership; without one, firecracker runs as our own
	// uid and owner-only already suffices.
	if cfg.SocketUID != 0 || cfg.SocketGID != 0 {
		if err := os.Chown(cfg.SocketPath, int(cfg.SocketUID), int(cfg.SocketGID)); err != nil {
			_ = ln.Close()
			_ = memF.Close()
			return nil, fmt.Errorf("memfault: chown socket: %w", err)
		}
	}
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		_ = memF.Close()
		return nil, fmt.Errorf("memfault: chmod socket: %w", err)
	}
	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		_ = ln.Close()
		_ = memF.Close()
		return nil, fmt.Errorf("memfault: create wake pipe: %w", err)
	}

	h := &Handler{
		ln:    ln,
		memF:  memF,
		sock:  cfg.SocketPath,
		log:   log.With("component", "memfault", "socket", cfg.SocketPath),
		wakeR: wakeR,
		wakeW: wakeW,
		done:  make(chan struct{}),
	}
	go h.run()
	return h, nil
}

// Served reports how many pages have been installed so far.
func (h *Handler) Served() uint64 { return h.served.Load() }

// Close stops the Handler and releases the socket, the userfaultfd,
// and the memory file. Idempotent; safe to call at any point of the
// handshake/serving lifecycle.
func (h *Handler) Close() error {
	h.closeOnce.Do(func() {
		_ = h.wakeW.Close() // wakes poll(2)
		_ = h.ln.Close()    // unblocks Accept if firecracker never connected
		<-h.done
		_ = h.wakeR.Close()
		_ = h.memF.Close()
		_ = os.Remove(h.sock)
	})
	return nil
}

func (h *Handler) run() {
	defer close(h.done)

	conn, err := h.ln.AcceptUnix()
	if err != nil {
		return // listener closed before firecracker connected
	}
	// Detach to a raw fd: the handshake needs recvmsg for SCM_RIGHTS,
	// and the serve loop polls the fd to notice firecracker exiting.
	connF, err := conn.File()
	_ = conn.Close()
	if err != nil {
		h.log.Error("uffd conn detach failed", "err", err)
		return
	}
	defer func() { _ = connF.Close() }()
	connFd := int(connF.Fd())

	uffd, regions, err := h.handshake(connFd)
	if err != nil {
		if !errors.Is(err, errClosed) {
			h.log.Error("uffd handshake failed", "err", err)
		}
		return
	}
	defer func() { _ = unix.Close(uffd) }()

	var guestBytes uint64
	for _, r := range regions {
		guestBytes += r.Size
	}
	h.log.Info("uffd handshake complete", "regions", len(regions), "guest_bytes", guestBytes)

	h.serveFaults(uffd, connFd, regions)
	h.log.Info("uffd handler exiting", "pages_served", h.served.Load())
}

// handshake receives Firecracker's guest-layout JSON plus the
// userfaultfd passed via SCM_RIGHTS. On success the caller owns the
// returned fd.
func (h *Handler) handshake(connFd int) (int, []guestRegionUffdMapping, error) {
	if err := h.waitReadable(connFd); err != nil {
		return -1, nil, err
	}
	buf := make([]byte, 16<<10)
	oob := make([]byte, unix.CmsgSpace(4*4)) // room for a few fds; we expect one
	n, oobn, _, _, err := unix.Recvmsg(connFd, buf, oob, 0)
	if err != nil {
		return -1, nil, fmt.Errorf("recvmsg: %w", err)
	}
	if n == 0 {
		return -1, nil, errors.New("connection closed before handshake")
	}

	var fds []int
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, nil, fmt.Errorf("parse control messages: %w", err)
	}
	for _, scm := range scms {
		got, err := unix.ParseUnixRights(&scm)
		if err != nil {
			continue
		}
		fds = append(fds, got...)
	}
	if len(fds) == 0 {
		return -1, nil, errors.New("no userfaultfd in handshake")
	}
	uffd := fds[0]
	for _, extra := range fds[1:] {
		_ = unix.Close(extra)
	}

	var regions []guestRegionUffdMapping
	if err := json.Unmarshal(buf[:n], &regions); err != nil {
		_ = unix.Close(uffd)
		return -1, nil, fmt.Errorf("parse guest layout %q: %w", buf[:n], err)
	}
	if len(regions) == 0 {
		_ = unix.Close(uffd)
		return -1, nil, errors.New("handshake described zero memory regions")
	}
	// poll(2) on a userfaultfd reports only POLLERR unless the fd is
	// non-blocking. Firecracker creates its uffd non-blocking already;
	// setting it here makes the serve loop correct regardless.
	if err := unix.SetNonblock(uffd, true); err != nil {
		_ = unix.Close(uffd)
		return -1, nil, fmt.Errorf("set uffd non-blocking: %w", err)
	}
	return uffd, regions, nil
}

// waitReadable polls fd until it is readable (or has hung up — the
// caller's read will observe that), or the Handler is closed.
func (h *Handler) waitReadable(fd int) error {
	fds := []unix.PollFd{
		{Fd: int32(fd), Events: unix.POLLIN},
		{Fd: int32(h.wakeR.Fd()), Events: unix.POLLIN},
	}
	for {
		for i := range fds {
			fds[i].Revents = 0
		}
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("poll: %w", err)
		}
		if fds[1].Revents != 0 {
			return errClosed
		}
		if fds[0].Revents != 0 {
			return nil
		}
	}
}

// serveFaults is the lab loop: poll → read uffd_msg → pread the page
// from the memory file → UFFDIO_COPY. It returns when the Handler is
// closed, firecracker exits (socket hangup), or serving fails.
func (h *Handler) serveFaults(uffd, connFd int, regions []guestRegionUffdMapping) {
	var maxPage uint64 = defaultPageSize
	for _, r := range regions {
		if ps := r.pageSize(); ps > maxPage {
			maxPage = ps
		}
	}
	// One read drains up to one pending fault message per vCPU; size
	// the buffer for a comfortable batch.
	msgBuf := make([]byte, 64*uffdMsgSize)
	pageBuf := make([]byte, maxPage)

	fds := []unix.PollFd{
		{Fd: int32(uffd), Events: unix.POLLIN},
		{Fd: int32(connFd), Events: 0}, // POLLHUP/POLLERR are always reported
		{Fd: int32(h.wakeR.Fd()), Events: unix.POLLIN},
	}
	for {
		for i := range fds {
			fds[i].Revents = 0
		}
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			h.log.Error("uffd poll failed", "err", err)
			return
		}
		if fds[2].Revents != 0 {
			return // Close() called
		}
		if fds[1].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return // firecracker exited; the VM (and its faults) are gone
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
				// Never treat this as "not ready": that would spin.
				h.log.Error("uffd poll error", "revents", fds[0].Revents)
				return
			}
			continue
		}

		n, err := unix.Read(uffd, msgBuf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			h.log.Error("uffd read failed", "err", err)
			return
		}
		for off := 0; off+uffdMsgSize <= n; off += uffdMsgSize {
			if msgBuf[off] != uffdEventPagefault {
				// Balloon remove/remap events — crucible configures
				// no balloon device, so just skip.
				continue
			}
			addr := binary.LittleEndian.Uint64(msgBuf[off+uffdMsgAddrOffset:])
			if err := h.servePage(uffd, addr, regions, pageBuf); err != nil {
				h.log.Error("serve page failed", "addr", fmt.Sprintf("%#x", addr), "err", err)
				return
			}
		}
	}
}

func (h *Handler) servePage(uffd int, addr uint64, regions []guestRegionUffdMapping, pageBuf []byte) error {
	var reg *guestRegionUffdMapping
	for i := range regions {
		r := &regions[i]
		if addr >= r.BaseHostVirtAddr && addr < r.BaseHostVirtAddr+r.Size {
			reg = r
			break
		}
	}
	if reg == nil {
		return fmt.Errorf("fault outside all registered regions")
	}
	pageSize := reg.pageSize()
	page := addr &^ (pageSize - 1)
	fileOff := int64(reg.Offset + (page - reg.BaseHostVirtAddr))

	b := pageBuf[:pageSize]
	n, err := h.memF.ReadAt(b, fileOff)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read memory file @%d: %w", fileOff, err)
	}
	// Short read at EOF (truncated/short file): the missing tail is
	// zero, matching what an eager file mapping would produce.
	for i := n; i < len(b); i++ {
		b[i] = 0
	}

	cp := uffdioCopy{
		Dst: page,
		Src: uint64(uintptr(unsafe.Pointer(&b[0]))),
		Len: pageSize,
	}
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioCopyIoctl, uintptr(unsafe.Pointer(&cp)))
		switch errno {
		case 0:
			h.served.Add(1)
			return nil
		case unix.EEXIST:
			// Two vCPUs faulted the same page; the first COPY already
			// installed it and woke all waiters.
			return nil
		case unix.EAGAIN:
			// The VM's memory map was changing mid-copy; retry.
			cp.Copy = 0
		default:
			return fmt.Errorf("UFFDIO_COPY dst=%#x: %w", page, errno)
		}
	}
}
