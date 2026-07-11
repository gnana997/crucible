// Package ingress routes inbound traffic to an app's current instance: a name
// resolver (A3) and, on top of it, the in-process proxy (A2, forthcoming).
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
)

// AppLookup resolves an app by its user-facing name. Satisfied directly by
// *app.Manager (GetByName).
type AppLookup interface {
	GetByName(name string) (api.AppResponse, error)
}

// InstanceLookup returns a sandbox instance's guest IP, or ("", false) when the
// instance is unknown or has no network.
type InstanceLookup interface {
	GuestIP(instanceID string) (string, bool)
}

// Target is a resolved upstream: the guest IP and port to dial.
type Target struct {
	GuestIP string
	Port    int
}

// Resolver maps a request hostname → the current instance of the named app.
type Resolver struct {
	apps      AppLookup
	instances InstanceLookup
	domain    string // proxy domain suffix (e.g. "apps.local"); "" = host is the app name
	ttl       time.Duration
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	instanceID string
	target     Target
	expires    time.Time
}

// NewResolver builds a resolver. domain is the --proxy-domain suffix (a leading
// dot is optional); ttl is the cache window (0 disables caching).
func NewResolver(apps AppLookup, instances InstanceLookup, domain string, ttl time.Duration) *Resolver {
	return &Resolver{
		apps:      apps,
		instances: instances,
		domain:    strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), "."),
		ttl:       ttl,
		now:       time.Now,
		cache:     map[string]cacheEntry{},
	}
}

// AppName extracts the app name from a request host: strip any :port, lowercase,
// trim a trailing dot, then remove the proxy-domain suffix. Returns "" if the
// host is not under the configured domain.
func (r *Resolver) AppName(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(h, ".")
	if r.domain == "" {
		return h
	}
	suffix := "." + r.domain
	if !strings.HasSuffix(h, suffix) {
		return ""
	}
	return strings.TrimSuffix(h, suffix)
}

// Resolve maps a request host to the current instance's guest IP + port, live
// (cached for ttl). ErrNoRoute for an unknown host/app or missing target port;
// ErrNoInstance when the app has no ready instance.
func (r *Resolver) Resolve(host string) (Target, error) {
	name := r.AppName(host)
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
	if resp.Status == nil || resp.Status.InstanceID == "" || resp.Status.Phase != "running" {
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
