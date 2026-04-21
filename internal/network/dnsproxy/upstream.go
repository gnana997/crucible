package dnsproxy

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// SystemResolvConf is the conventional path for the system's
// stub resolver config. Parameterized only so tests can swap it
// for a tmpfile.
const SystemResolvConf = "/etc/resolv.conf"

// cloudflareFallback is the hardcoded resolver we fall back to
// when --dns-upstream=system resolution fails (file missing,
// unreadable, or contains no usable nameserver line).
//
// 1.1.1.1 was chosen because it's public, fast, privacy-aware
// (no query logging per Cloudflare's published policy), and
// responds to both plain DNS and DoH/DoT — so the fallback
// doesn't silently change behavior if we later move the proxy
// to an encrypted transport.
const cloudflareFallback = "1.1.1.1:53"

// ResolveUpstream turns a --dns-upstream flag value into an
// "ip:port" string the miekg/dns client can dial. Accepts:
//
//   - ""             → treated as "system"
//   - "system"       → read SystemResolvConf, first nameserver,
//                      append :53. Falls back to cloudflareFallback
//                      on any failure (the reason is returned via
//                      the second error; the caller typically logs
//                      it at WARN and continues).
//   - "1.2.3.4"      → "1.2.3.4:53"
//   - "1.2.3.4:5353" → unchanged
//
// The first return is always a dialable address (never empty on
// a nil error); the second is a non-fatal warning from the
// "system" path, meant for structured logging rather than
// failing startup.
func ResolveUpstream(spec string) (string, error) {
	return resolveUpstreamWithPath(spec, SystemResolvConf)
}

// resolveUpstreamWithPath is the testable core — same contract
// as ResolveUpstream but reads from an explicit path. Tests pass
// a tmpfile; production calls through ResolveUpstream.
func resolveUpstreamWithPath(spec, resolvPath string) (string, error) {
	if spec == "" {
		spec = "system"
	}
	if spec == "system" {
		ns, err := firstNameserver(resolvPath)
		if err != nil {
			// Non-fatal: fall back to Cloudflare and return the
			// underlying reason so the caller can log it.
			return cloudflareFallback, fmt.Errorf("system resolver lookup failed (falling back to %s): %w",
				cloudflareFallback, err)
		}
		return net.JoinHostPort(ns, "53"), nil
	}
	// Explicit: accept either "ip" or "ip:port".
	host, port, err := net.SplitHostPort(spec)
	if err == nil {
		if _, perr := netip.ParseAddr(host); perr != nil {
			return "", fmt.Errorf("upstream %q: invalid host %q", spec, host)
		}
		// Validate port: must be a positive integer <= 65535.
		// Without this we'd accept "8.8.8.8:abc" because
		// SplitHostPort is content-agnostic about the port token.
		n, perr := strconv.Atoi(port)
		if perr != nil || n <= 0 || n > 65535 {
			return "", fmt.Errorf("upstream %q: invalid port %q", spec, port)
		}
		return net.JoinHostPort(host, port), nil
	}
	// SplitHostPort failed — treat the whole string as an IP.
	if _, perr := netip.ParseAddr(spec); perr != nil {
		return "", fmt.Errorf("upstream %q: not a valid IP or ip:port: %v", spec, err)
	}
	return net.JoinHostPort(spec, "53"), nil
}

// firstNameserver scans a resolv.conf-formatted file and returns
// the first nameserver directive's value. Returns an error if
// the file can't be opened or contains no nameserver lines.
//
// Respects the same minimal parsing rules as resolv.conf:
//
//   - Blank lines and lines starting with '#' or ';' are skipped.
//   - A "nameserver" line has the IP as the second whitespace-
//     separated token.
//   - Only the first nameserver is returned (by design — see
//     docs/network.md's "single upstream" decision).
func firstNameserver(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ns := fields[1]
		if _, err := netip.ParseAddr(ns); err != nil {
			// Corrupt line — skip, try the next.
			continue
		}
		return ns, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no nameserver directive found")
}
