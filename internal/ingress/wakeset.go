package ingress

import (
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

// WakeForwarderSet manages the app-scoped waking TCP forwarders: for every
// scale-to-zero app that publishes a host port, one WakingForwarder per mapping
// that fronts the host port, wakes the app on connect, and — unlike per-instance
// publish, which dies with the VM — survives the app's sleep. It is kept in sync
// with desired state by ReconcilePorts (satisfying app.PortReconciler), called on
// each reconcile pass, so create/update/delete/stop and restart adoption all flow
// through one diff.
type WakeForwarderSet struct {
	resolver *Resolver
	waker    Waker
	activity *ActivityTracker
	log      *slog.Logger

	mu   sync.Mutex
	apps map[string]*appForwarders // app name → its bound forwarders + signature
}

type appForwarders struct {
	sig  string // signature of the bound mapping set; a change forces a rebind
	fwds []*WakingForwarder
}

// NewWakeForwarderSet builds an empty set. resolver maps an app name → its
// current instance (ResolveName); waker (the app manager) wakes a slept app;
// activity is the shared idle-monitor tracker.
func NewWakeForwarderSet(resolver *Resolver, waker Waker, activity *ActivityTracker, log *slog.Logger) *WakeForwarderSet {
	if log == nil {
		log = slog.Default()
	}
	return &WakeForwarderSet{
		resolver: resolver,
		waker:    waker,
		activity: activity,
		log:      log.With("component", "l4wake"),
		apps:     map[string]*appForwarders{},
	}
}

// WakesOnTCP reports whether an app's host port should be fronted by an
// app-scoped waking forwarder rather than bound per instance: a scale-to-zero app
// (min_scale 0, idle_timeout > 0) that publishes at least one host port. Such an
// app's instance must NOT bind the port itself (the forwarder owns it, so it
// survives sleep) — the instantiator suppresses instance publish for these.
func WakesOnTCP(spec api.AppSpec) bool {
	sp := spec.Sleep
	return sp != nil && sp.MinScale == 0 && sp.IdleTimeoutSec > 0 && len(spec.Publish) > 0
}

type fwdSpec struct {
	hostAddr  string
	guestPort int
}

func desiredForwarders(spec api.AppSpec) []fwdSpec {
	out := make([]fwdSpec, 0, len(spec.Publish))
	for _, p := range spec.Publish {
		if p.HostPort <= 0 || p.GuestPort <= 0 {
			continue
		}
		out = append(out, fwdSpec{
			hostAddr:  net.JoinHostPort(p.HostIP, strconv.Itoa(p.HostPort)),
			guestPort: p.GuestPort,
		})
	}
	return out
}

// reapPolicy derives the forwarder's idle-connection behavior from an app's sleep
// policy: how long a connection may sit byte-idle before it's reaped, and whether
// to enable TCP keepalive. KeepConnections → never reap on silence (pub/sub),
// keepalive on. Otherwise reap at ConnIdleTimeoutSec, defaulting to IdleTimeoutSec.
// WakesOnTCP guarantees Sleep is non-nil with IdleTimeoutSec > 0.
func reapPolicy(spec api.AppSpec) (reapIdle time.Duration, keepAlive bool) {
	sp := spec.Sleep
	if sp.KeepConnections {
		return 0, true
	}
	secs := sp.ConnIdleTimeoutSec
	if secs <= 0 {
		secs = sp.IdleTimeoutSec
	}
	return time.Duration(secs) * time.Second, false
}

func signatureOf(fs []fwdSpec, reapIdle time.Duration, keepAlive bool) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = f.hostAddr + ">" + strconv.Itoa(f.guestPort)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",") + "|reap=" + reapIdle.String() + "|keep=" + strconv.FormatBool(keepAlive)
}

// ReconcilePorts binds a waking forwarder per publish mapping for each
// scale-to-zero published app, and closes forwarders for apps that no longer
// qualify (deleted, stopped, or de-scaled) or whose mapping set changed. Called
// on every reconcile pass; an unchanged set is a cheap no-op. Closing a forwarder
// force-drops its live connections (app gone → its connections go too), so this
// never blocks the reconcile loop on a long-lived connection.
func (s *WakeForwarderSet) ReconcilePorts(specs []api.AppSpec) {
	type want struct {
		sig       string
		fwds      []fwdSpec
		reapIdle  time.Duration
		keepAlive bool
	}
	desired := make(map[string]want, len(specs))
	for _, sp := range specs {
		if !WakesOnTCP(sp) {
			continue
		}
		fs := desiredForwarders(sp)
		if len(fs) == 0 {
			continue
		}
		reapIdle, keepAlive := reapPolicy(sp)
		desired[sp.Name] = want{sig: signatureOf(fs, reapIdle, keepAlive), fwds: fs, reapIdle: reapIdle, keepAlive: keepAlive}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Close forwarders whose app is gone or whose mapping set changed.
	for name, cur := range s.apps {
		if d, ok := desired[name]; !ok || d.sig != cur.sig {
			for _, f := range cur.fwds {
				f.Close()
			}
			delete(s.apps, name)
		}
	}

	// Bind forwarders for newly-qualifying (or changed) apps.
	for name, d := range desired {
		if _, ok := s.apps[name]; ok {
			continue // already bound with the same signature
		}
		fwds := make([]*WakingForwarder, 0, len(d.fwds))
		bound := true
		for _, fs := range d.fwds {
			f, err := NewWakingForwarder(WakingForwarderConfig{
				HostAddr:  fs.hostAddr,
				AppName:   name,
				GuestPort: fs.guestPort,
				Resolver:  s.resolver,
				Waker:     s.waker,
				Activity:  s.activity,
				ReapIdle:  d.reapIdle,
				KeepAlive: d.keepAlive,
				Log:       s.log,
			})
			if err != nil {
				// A busy host port, say. Roll back the partial bind and leave the
				// app unrecorded so the next reconcile pass retries.
				s.log.Warn("bind failed", "app", name, "host", fs.hostAddr, "err", err)
				bound = false
				break
			}
			fwds = append(fwds, f)
		}
		if !bound {
			for _, f := range fwds {
				f.Close()
			}
			continue
		}
		s.apps[name] = &appForwarders{sig: d.sig, fwds: fwds}
	}
}

// Close tears down every forwarder (daemon shutdown).
func (s *WakeForwarderSet) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, af := range s.apps {
		for _, f := range af.fwds {
			f.Close()
		}
		delete(s.apps, name)
	}
}
