// Package ingress routes inbound traffic to an app's current instance: a name
// resolver and, on top of it, the in-process proxy.
//
// This file is the resolver. It maps a request hostname to the live guest IP +
// port of the named app's *current* instance, resolved fresh (with a small TTL
// cache) so the proxy can never route to a stale address — instance IPs change
// on every re-create/fork, and the app object is the source of truth for which
// instance is current.
package ingress

import (
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

var (
	// ErrNoRoute means the hostname maps to no known app (unknown name, or not
	// under the proxy domain, or the app has no target port).
	ErrNoRoute = errors.New("ingress: no app for host")
	// ErrNoInstance means the app exists but has no ready instance to route to
	// (pending, crashlooping, stopped, or the IP isn't known yet).
	ErrNoInstance = errors.New("ingress: app has no ready instance")
	// ErrAsleep means the app is asleep (or mid-wake): it has a snapshot to
	// restore but no running VMM. The proxy treats this as "trigger a wake and
	// hold the request", not as a hard error — distinct from ErrNoInstance,
	// which is a genuinely unroutable app.
	ErrAsleep = errors.New("ingress: app is asleep")
)

// AppLookup resolves an app by its user-facing name or an attached custom
// domain. Satisfied directly by *app.Manager.
type AppLookup interface {
	GetByName(name string) (api.AppResponse, error)
	// GetByDomain returns the app a custom domain is attached to, and whether
	// one is. Consulted only for external hosts that aren't <app>.<proxy-domain>.
	GetByDomain(domain string) (api.AppResponse, bool)
}

// InstanceLookup returns a sandbox instance's guest IP, or ("", false) when the
// instance is unknown or has no network.
type InstanceLookup interface {
	GuestIP(instanceID string) (string, bool)
}

// Target is a resolved upstream: one instance's guest IP and port to dial.
// InstanceID identifies the instance so the load balancer can key per-instance
// state (in-flight count, ejection) — zero for a single-instance resolve.
type Target struct {
	InstanceID string
	GuestIP    string
	Port       int
}

// Resolver maps a request hostname → the current instance of the named app.
type Resolver struct {
	apps         AppLookup
	instances    InstanceLookup
	domain       string // proxy domain suffix (e.g. "apps.local"); "" = host is the app name
	internalZone string // app→app zone suffix (e.g. "internal"); "" = app→app disabled
	ttl          time.Duration
	now          func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	instanceID string
	target     Target
	expires    time.Time
}

// NewResolver builds a resolver. domain is the --proxy-domain suffix (a leading
// dot is optional); internalZone is the app→app suffix (e.g. "internal", ""
// disables app→app resolution); ttl is the cache window (0 disables caching).
func NewResolver(apps AppLookup, instances InstanceLookup, domain, internalZone string, ttl time.Duration) *Resolver {
	norm := func(s string) string {
		return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), ".")
	}
	return &Resolver{
		apps:         apps,
		instances:    instances,
		domain:       norm(domain),
		internalZone: norm(internalZone),
		ttl:          ttl,
		now:          time.Now,
		cache:        map[string]cacheEntry{},
	}
}

// normHost strips any :port, lowercases, and trims a trailing dot.
func normHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimSuffix(h, ".")
}

// AppName extracts the app name from an external request host: normalize, then
// remove the proxy-domain suffix. Returns "" if the host is not under the
// configured domain (when domain is "", the whole host is the app name).
func (r *Resolver) AppName(host string) string {
	h := normHost(host)
	if r.domain == "" {
		return h
	}
	suffix := "." + r.domain
	if !strings.HasSuffix(h, suffix) {
		return ""
	}
	return strings.TrimSuffix(h, suffix)
}

// appNameForHost maps an external request host to an app name: the proxy-domain
// subdomain (web.apps.local → web), or, failing that, a custom domain attached
// to an app (shop.acme.com → the app it's bound to). "" when neither matches.
func (r *Resolver) appNameForHost(host string) string {
	if name := r.AppName(host); name != "" {
		return name
	}
	if resp, ok := r.apps.GetByDomain(normHost(host)); ok {
		return resp.Name
	}
	return ""
}

// AppNameInternal extracts the app name from an app→app host in the internal
// zone (e.g. "backend.internal" → "backend"). Returns "" when app→app is
// disabled (internalZone == "") or the host is not under the internal zone.
func (r *Resolver) AppNameInternal(host string) string {
	if r.internalZone == "" {
		return ""
	}
	suffix := "." + r.internalZone
	h := normHost(host)
	if !strings.HasSuffix(h, suffix) {
		return ""
	}
	return strings.TrimSuffix(h, suffix)
}

// Resolve maps an external request host to the current instance's guest IP +
// port, live (cached for ttl). ErrNoRoute for an unknown host/app or missing
// target port; ErrNoInstance when the app has no ready instance.
func (r *Resolver) Resolve(host string) (Target, error) {
	return r.resolve(r.appNameForHost(host))
}

// ResolveInternal is Resolve for an app→app host in the internal zone
// (backend.internal → the backend app's current instance). Same wake/routing
// semantics as Resolve; the caller-authorization check lives at the proxy
// handler (which knows the calling guest), not here.
func (r *Resolver) ResolveInternal(host string) (Target, error) {
	return r.resolve(r.AppNameInternal(host))
}

// TLSModePassthrough is the AppSpec.TLSMode value that keeps the proxy piping
// the raw TLS stream to the guest instead of terminating it.
const TLSModePassthrough = "passthrough"

// TLSTerminate reports whether the proxy should TERMINATE TLS for this SNI (vs
// pass it through to the guest) — and doubles as the ACME on-demand gate: a cert
// is only ever obtained for an SNI this returns true for. True when the SNI maps
// to a known app (by proxy-domain name or attached custom domain) that has not
// opted into passthrough. An unknown SNI returns false — nothing to terminate or
// issue for; the passthrough path resolves-and-fails cleanly.
func (r *Resolver) TLSTerminate(sni string) bool {
	name := r.appNameForHost(sni)
	if name == "" {
		return false
	}
	resp, err := r.apps.GetByName(name)
	if err != nil {
		return false
	}
	return resp.TLSMode != TLSModePassthrough
}

// ResolveName resolves an app directly by name (not a request host) to its
// current instance target. Used by the L4 waking forwarder, which already knows
// which app it fronts and only needs the live guest IP (it supplies its own
// guest port from the publish mapping). Same phase semantics as Resolve —
// ErrAsleep for a slept/waking app (the forwarder wakes and re-resolves).
func (r *Resolver) ResolveName(name string) (Target, error) {
	return r.resolve(name)
}

// ResolveSet maps an external request host to the app's full endpoint set — the
// live guest IP:port of every running instance (primary + replicas) — for the
// load balancer to pick from. Same phase semantics as Resolve (ErrAsleep for a
// slept/waking app, ErrNoInstance when nothing is ready). Not cached: it reads
// the app's own instances, so there's no cross-tenant /30-reuse window.
func (r *Resolver) ResolveSet(host string) ([]Target, error) {
	return r.resolveSet(r.appNameForHost(host))
}

// ResolveSetInternal is ResolveSet for an app→app host in the internal zone.
func (r *Resolver) ResolveSetInternal(host string) ([]Target, error) {
	return r.resolveSet(r.AppNameInternal(host))
}

func (r *Resolver) resolveSet(name string) ([]Target, error) {
	if name == "" {
		return nil, ErrNoRoute
	}
	resp, err := r.apps.GetByName(name)
	if err != nil {
		return nil, ErrNoRoute
	}
	if resp.Status == nil {
		return nil, ErrNoInstance
	}
	switch resp.Status.Phase {
	case "asleep", "waking":
		return nil, ErrAsleep
	case "running":
		// routable
	default:
		return nil, ErrNoInstance
	}
	port := resp.Port
	if port == 0 {
		port = firstPublishGuestPort(resp.Publish)
	}
	if port <= 0 {
		return nil, ErrNoRoute
	}
	set := make([]Target, 0, len(resp.Status.Instances))
	for _, in := range resp.Status.Instances {
		if ip, live := r.instances.GuestIP(in.InstanceID); live && ip != "" {
			set = append(set, Target{InstanceID: in.InstanceID, GuestIP: ip, Port: port})
		}
	}
	if len(set) == 0 {
		return nil, ErrNoInstance // running but no live instance IP yet
	}
	return set, nil
}

// resolve maps an already-extracted app name to its current instance target.
// The name→instance cache is keyed by app name, so it is shared across the
// external and internal zones (same app, same target).
func (r *Resolver) resolve(name string) (Target, error) {
	if name == "" {
		return Target{}, ErrNoRoute
	}

	// Cache hit: trust it only if the cached instance is STILL alive with the
	// same IP. Instance ids (sbx_…) are unique and never reused, so re-checking
	// the sandbox manager on the cached id can never return a *different* app's
	// guest — a freed /30 re-leased to another app resolves under that app's own
	// (different) instance id. This closes the /30-reuse tenant-crossover window
	// while still saving the app-store lookup on the hot path.
	if r.ttl > 0 {
		r.mu.Lock()
		e, ok := r.cache[name]
		r.mu.Unlock()
		if ok && r.now().Before(e.expires) {
			if ip, live := r.instances.GuestIP(e.instanceID); live && ip == e.target.GuestIP {
				return e.target, nil
			}
			r.mu.Lock()
			delete(r.cache, name) // instance gone / IP changed → re-resolve
			r.mu.Unlock()
		}
	}

	resp, err := r.apps.GetByName(name)
	if err != nil {
		return Target{}, ErrNoRoute // unknown app (or store error) → no route
	}
	if resp.Status == nil {
		return Target{}, ErrNoInstance
	}
	// Phase is authoritative first: an asleep/waking app is wakeable even with no
	// instance id — after a daemon restart it is re-adopted asleep with no live
	// instance, and the wake creates a fresh one. Only "running" needs an
	// instance to route to.
	switch resp.Status.Phase {
	case "asleep", "waking":
		return Target{}, ErrAsleep // wakeable: the proxy triggers a wake
	case "running":
		// routable — fall through to the instance lookup
	default: // pending, crashlooping, stopped
		return Target{}, ErrNoInstance
	}
	if resp.Status.InstanceID == "" {
		return Target{}, ErrNoInstance
	}
	ip, ok := r.instances.GuestIP(resp.Status.InstanceID)
	if !ok || ip == "" {
		return Target{}, ErrNoInstance
	}
	port := resp.Port
	if port == 0 {
		port = firstPublishGuestPort(resp.Publish)
	}
	if port <= 0 {
		return Target{}, ErrNoRoute // misconfigured: nowhere to forward
	}

	t := Target{GuestIP: ip, Port: port}
	if r.ttl > 0 {
		r.mu.Lock()
		r.cache[name] = cacheEntry{instanceID: resp.Status.InstanceID, target: t, expires: r.now().Add(r.ttl)}
		r.mu.Unlock()
	}
	return t, nil
}

func firstPublishGuestPort(pms []api.PortMapping) int {
	for _, p := range pms {
		if p.GuestPort > 0 {
			return p.GuestPort
		}
	}
	return 0
}
