// Package capture streams a live packet capture of a sandbox's traffic as a
// standard pcap stream. It runs host-side, on the sandbox's host-veth interface
// in the root netns (which carries all the guest's traffic), so it needs NO
// in-guest binary and works for distroless/scratch images.
//
// It shells out to the host's tcpdump (for correct pcap output + full BPF filter
// support without a CGO libpcap dependency), bounded by hard byte and duration
// caps so a capture can never fill disk or run unbounded.
package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// Defaults bound every capture unless the caller sets tighter values.
const (
	DefaultMaxBytes int64 = 50 << 20 // 50 MiB
	DefaultMaxDur         = 60 * time.Second
	DefaultSnaplen        = 262144 // effectively whole-packet
)

// ErrNoTcpdump is returned when the host has no tcpdump on PATH.
var ErrNoTcpdump = errors.New("capture: tcpdump not found on host PATH")

// bpfSafe permits a conservative BPF-expression charset and requires an
// alphanumeric first char, so a filter can never inject a tcpdump option (we
// exec tcpdump directly — no shell — and pass the filter as a single argument).
var bpfSafe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 .:/()\[\]!<>=&|-]*$`)

// ValidFilter reports whether s is a safe BPF filter expression (empty = no
// filter, allowed).
func ValidFilter(s string) bool { return s == "" || bpfSafe.MatchString(s) }

// Options configures one capture.
type Options struct {
	Iface    string        // host interface to capture on (the sandbox's host veth)
	Filter   string        // BPF expression; must pass ValidFilter
	Snaplen  int           // bytes captured per packet; <=0 → DefaultSnaplen
	MaxBytes int64         // hard cap on streamed pcap bytes; <=0 → DefaultMaxBytes
	MaxDur   time.Duration // hard cap on duration; <=0 → DefaultMaxDur
}

func (o *Options) applyDefaults() {
	if o.Snaplen <= 0 {
		o.Snaplen = DefaultSnaplen
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxDur <= 0 {
		o.MaxDur = DefaultMaxDur
	}
}

// Normalized returns a copy of o with defaults applied — handy for logging the
// effective caps before Run.
func (o Options) Normalized() Options {
	(&o).applyDefaults()
	return o
}

// Available reports whether host-side capture is possible (tcpdump on PATH).
func Available() bool {
	_, err := exec.LookPath("tcpdump")
	return err == nil
}

// tcpdumpArgs builds the argv. Exported-for-test via Args.
func tcpdumpArgs(o Options) []string {
	args := []string{"-i", o.Iface, "-w", "-", "-U", "-n", "-s", strconv.Itoa(o.Snaplen)}
	if o.Filter != "" {
		args = append(args, o.Filter) // single arg — validated, no option injection
	}
	return args
}

// Run streams pcap from tcpdump on o.Iface to w until a cap is hit, the client
// disconnects (w errors), or ctx is done. A cap-hit / disconnect / deadline is a
// clean stop (nil); a failure to start tcpdump is an error.
func Run(ctx context.Context, o Options, w io.Writer) error {
	if o.Iface == "" {
		return errors.New("capture: iface required")
	}
	if !ValidFilter(o.Filter) {
		return fmt.Errorf("capture: invalid filter %q", o.Filter)
	}
	o.applyDefaults()
	if _, err := exec.LookPath("tcpdump"); err != nil {
		return ErrNoTcpdump
	}

	ctx, cancel := context.WithTimeout(ctx, o.MaxDur)
	defer cancel()

	cmd := exec.CommandContext(ctx, "tcpdump", tcpdumpArgs(o)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("capture: start tcpdump: %w", err)
	}

	cw := &capWriter{w: w, limit: o.MaxBytes, cancel: cancel}
	_, copyErr := io.Copy(cw, stdout)
	_ = cmd.Wait()

	if cw.hitLimit || errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
		return nil // bounded stop / client gone — not an error
	}
	return copyErr
}

// capWriter forwards to w until limit bytes, then cancels the capture.
type capWriter struct {
	w        io.Writer
	limit    int64
	written  int64
	hitLimit bool
	cancel   context.CancelFunc
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.written >= c.limit {
		c.hitLimit = true
		c.cancel()
		return 0, io.ErrShortWrite
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	if c.written >= c.limit {
		c.hitLimit = true
		c.cancel()
	}
	return n, err
}
