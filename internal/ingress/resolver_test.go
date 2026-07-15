package ingress

import (
	"errors"
	"testing"
	"time"

	"github.com/gnana997/crucible/sdk/api"
)

type fakeApps struct {
	apps map[string]api.AppResponse
}

func (f fakeApps) GetByName(name string) (api.AppResponse, error) {
	a, ok := f.apps[name]
	if !ok {
		return api.AppResponse{}, errors.New("app: not found")
	}
	return a, nil
}

func (f fakeApps) GetByDomain(domain string) (api.AppResponse, bool) {
	for _, a := range f.apps {
		for _, d := range a.Domains {
			if d == domain {
				return a, true
			}
		}
	}
	return api.AppResponse{}, false
}

type fakeInstances struct{ ips map[string]string }

func (f fakeInstances) GuestIP(id string) (string, bool) {
	ip, ok := f.ips[id]
	return ip, ok
}

func runningApp(name string, port int, instance string, publish ...api.PortMapping) api.AppResponse {
	r := api.AppResponse{ID: "app_" + name}
	r.Name = name
	r.Port = port
	r.Publish = publish
	r.Status = &api.AppStatus{InstanceID: instance, Phase: "running"}
	if instance != "" {
		r.Status.Instances = []api.InstanceStatus{{InstanceID: instance}}
	}
	return r
}

func TestAppName(t *testing.T) {
	r := NewResolver(nil, nil, "apps.local", "", 0)
	cases := map[string]string{
		"web.apps.local":    "web",
		"web.apps.local:80": "web",
		"WEB.APPS.LOCAL":    "web",
		"web.apps.local.":   "web",
		"other.example.com": "", // not under the domain
		"apps.local":        "", // domain itself, no app label
		"a.b.apps.local":    "a.b",
	}
	for host, want := range cases {
		if got := r.AppName(host); got != want {
			t.Errorf("AppName(%q) = %q, want %q", host, got, want)
		}
	}

	// No domain → the host is the app name.
	bare := NewResolver(nil, nil, "", "", 0)
	if got := bare.AppName("web:8080"); got != "web" {
		t.Errorf("bare AppName = %q, want web", got)
	}
}

func TestAppNameInternal(t *testing.T) {
	r := NewResolver(nil, nil, "apps.local", "internal", 0)
	cases := map[string]string{
		"backend.internal":    "backend",
		"backend.internal:80": "backend",
		"BACKEND.Internal.":   "backend",
		"a.b.internal":        "a.b",
		"web.apps.local":      "", // external zone, not internal
		"internal":            "", // bare zone, no app label
		"backend.example.com": "",
	}
	for host, want := range cases {
		if got := r.AppNameInternal(host); got != want {
			t.Errorf("AppNameInternal(%q) = %q, want %q", host, got, want)
		}
	}
	// Disabled internal zone → always "".
	off := NewResolver(nil, nil, "apps.local", "", 0)
	if got := off.AppNameInternal("backend.internal"); got != "" {
		t.Errorf("disabled zone: AppNameInternal = %q, want \"\"", got)
	}
}

func TestResolveInternal(t *testing.T) {
	apps := fakeApps{apps: map[string]api.AppResponse{"backend": runningApp("backend", 8080, "sbx_9")}}
	inst := fakeInstances{ips: map[string]string{"sbx_9": "10.20.0.6"}}
	r := NewResolver(apps, inst, "apps.local", "internal", 0)

	// In-zone app→app host resolves to the callee's instance.
	tg, err := r.ResolveInternal("backend.internal")
	if err != nil {
		t.Fatalf("ResolveInternal: %v", err)
	}
	if tg.GuestIP != "10.20.0.6" || tg.Port != 8080 {
		t.Errorf("target = %+v, want 10.20.0.6:8080", tg)
	}
	// An external host is not in the internal zone → no route.
	if _, err := r.ResolveInternal("backend.apps.local"); !errors.Is(err, ErrNoRoute) {
		t.Errorf("ResolveInternal(external host) err = %v, want ErrNoRoute", err)
	}
	// The external Resolve must NOT accept the .internal zone.
	if _, err := r.Resolve("backend.internal"); !errors.Is(err, ErrNoRoute) {
		t.Errorf("Resolve(.internal) err = %v, want ErrNoRoute", err)
	}
}

func TestResolveHappyPath(t *testing.T) {
	apps := fakeApps{apps: map[string]api.AppResponse{"web": runningApp("web", 80, "sbx_1")}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2"}}
	r := NewResolver(apps, inst, "apps.local", "", 0)

	tg, err := r.Resolve("web.apps.local")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tg.GuestIP != "10.20.0.2" || tg.Port != 80 {
		t.Errorf("target = %+v, want 10.20.0.2:80", tg)
	}
}

func TestResolveCustomDomain(t *testing.T) {
	web := runningApp("web", 80, "sbx_1")
	web.Domains = []string{"shop.acme.com"}
	apps := fakeApps{apps: map[string]api.AppResponse{"web": web}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2"}}
	r := NewResolver(apps, inst, "apps.local", "", 0)

	// A custom domain (not under the proxy zone) routes to its app.
	tg, err := r.Resolve("shop.acme.com")
	if err != nil {
		t.Fatalf("Resolve(custom domain): %v", err)
	}
	if tg.GuestIP != "10.20.0.2" || tg.Port != 80 {
		t.Errorf("custom-domain target = %+v, want 10.20.0.2:80", tg)
	}
	// TLS termination is on for a terminate-mode custom domain, off for an
	// unattached one.
	if !r.TLSTerminate("shop.acme.com") {
		t.Error("TLSTerminate(attached custom domain) = false, want true (issues a cert)")
	}
	if r.TLSTerminate("random.example.org") {
		t.Error("TLSTerminate(unregistered domain) = true, want false (never issue a stray cert)")
	}
}

// A custom domain that sits UNDER the proxy zone (shop.apps.local attached to
// app "web") must still route to "web" — not be shadowed by the nonexistent
// <app="shop">.apps.local interpretation.
func TestResolveCustomDomainUnderProxyZone(t *testing.T) {
	web := runningApp("web", 80, "sbx_1")
	web.Domains = []string{"shop.apps.local"}
	apps := fakeApps{apps: map[string]api.AppResponse{"web": web}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2"}}
	r := NewResolver(apps, inst, "apps.local", "", 0)

	tg, err := r.Resolve("shop.apps.local")
	if err != nil {
		t.Fatalf("Resolve(shop.apps.local): %v (should fall through to the custom-domain lookup)", err)
	}
	if tg.GuestIP != "10.20.0.2" {
		t.Errorf("target = %+v, want the 'web' app's guest", tg)
	}
	// The real <app>.<proxy-domain> still works.
	if _, err := r.Resolve("web.apps.local"); err != nil {
		t.Errorf("Resolve(web.apps.local): %v", err)
	}
}

func TestResolveErrors(t *testing.T) {
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2"}}

	// Unknown host / app → ErrNoRoute.
	r := NewResolver(fakeApps{apps: map[string]api.AppResponse{}}, inst, "apps.local", "", 0)
	if _, err := r.Resolve("nope.apps.local"); !errors.Is(err, ErrNoRoute) {
		t.Errorf("unknown app err = %v, want ErrNoRoute", err)
	}
	// Not under the proxy domain → ErrNoRoute.
	if _, err := r.Resolve("web.example.com"); !errors.Is(err, ErrNoRoute) {
		t.Errorf("off-domain err = %v, want ErrNoRoute", err)
	}

	// App present but no ready instance → ErrNoInstance.
	noInst := runningApp("web", 80, "") // empty instance id
	r2 := NewResolver(fakeApps{apps: map[string]api.AppResponse{"web": noInst}}, inst, "apps.local", "", 0)
	if _, err := r2.Resolve("web.apps.local"); !errors.Is(err, ErrNoInstance) {
		t.Errorf("no-instance err = %v, want ErrNoInstance", err)
	}

	// Instance id set but IP unknown → ErrNoInstance.
	unknownIP := runningApp("web", 80, "sbx_missing")
	r3 := NewResolver(fakeApps{apps: map[string]api.AppResponse{"web": unknownIP}}, inst, "apps.local", "", 0)
	if _, err := r3.Resolve("web.apps.local"); !errors.Is(err, ErrNoInstance) {
		t.Errorf("unknown-ip err = %v, want ErrNoInstance", err)
	}
}

// TestResolveAsleep: asleep/waking apps (instance id kept, VMM stopped)
// resolve to the distinct ErrAsleep — the proxy's signal to wake and hold —
// while other non-running phases stay ErrNoInstance.
func TestResolveAsleep(t *testing.T) {
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2"}}
	phased := func(phase string) api.AppResponse {
		a := runningApp("web", 80, "sbx_1")
		a.Status.Phase = phase
		return a
	}
	for _, phase := range []string{"asleep", "waking"} {
		r := NewResolver(fakeApps{apps: map[string]api.AppResponse{"web": phased(phase)}}, inst, "apps.local", "", 0)
		if _, err := r.Resolve("web.apps.local"); !errors.Is(err, ErrAsleep) {
			t.Errorf("phase %q err = %v, want ErrAsleep", phase, err)
		}
	}
	for _, phase := range []string{"pending", "crashlooping", "stopped"} {
		r := NewResolver(fakeApps{apps: map[string]api.AppResponse{"web": phased(phase)}}, inst, "apps.local", "", 0)
		if _, err := r.Resolve("web.apps.local"); !errors.Is(err, ErrNoInstance) {
			t.Errorf("phase %q err = %v, want ErrNoInstance", phase, err)
		}
	}

	// A re-adopted asleep app (after a daemon restart) has NO instance id but
	// is still wakeable → ErrAsleep, not ErrNoInstance.
	reAdopted := phased("asleep")
	reAdopted.Status.InstanceID = ""
	r := NewResolver(fakeApps{apps: map[string]api.AppResponse{"web": reAdopted}}, inst, "apps.local", "", 0)
	if _, err := r.Resolve("web.apps.local"); !errors.Is(err, ErrAsleep) {
		t.Errorf("re-adopted asleep (no instance id) err = %v, want ErrAsleep", err)
	}
}

func TestResolvePortFallback(t *testing.T) {
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2"}}

	// Port 0 → fall back to the first published guest port.
	fell := runningApp("web", 0, "sbx_1", api.PortMapping{HostPort: 8080, GuestPort: 8080})
	r := NewResolver(fakeApps{apps: map[string]api.AppResponse{"web": fell}}, inst, "apps.local", "", 0)
	tg, err := r.Resolve("web.apps.local")
	if err != nil || tg.Port != 8080 {
		t.Fatalf("port fallback = %+v, %v; want port 8080", tg, err)
	}

	// Port 0 and nothing published → ErrNoRoute (nowhere to forward).
	none := runningApp("web", 0, "sbx_1")
	r2 := NewResolver(fakeApps{apps: map[string]api.AppResponse{"web": none}}, inst, "apps.local", "", 0)
	if _, err := r2.Resolve("web.apps.local"); !errors.Is(err, ErrNoRoute) {
		t.Errorf("no-port err = %v, want ErrNoRoute", err)
	}
}

func TestResolveCacheWindow(t *testing.T) {
	apps := fakeApps{apps: map[string]api.AppResponse{"web": runningApp("web", 80, "sbx_1")}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2", "sbx_2": "10.20.0.9"}}
	r := NewResolver(apps, inst, "apps.local", "", time.Second)
	clk := time.Unix(0, 0).UTC()
	r.now = func() time.Time { return clk }

	if tg, _ := r.Resolve("web.apps.local"); tg.GuestIP != "10.20.0.2" {
		t.Fatalf("first resolve = %+v", tg)
	}
	// The instance changes, but within the TTL the cached target still serves.
	apps.apps["web"] = runningApp("web", 80, "sbx_2")
	if tg, _ := r.Resolve("web.apps.local"); tg.GuestIP != "10.20.0.2" {
		t.Errorf("within TTL = %+v, want cached 10.20.0.2", tg)
	}
	// Past the TTL, the resolver re-resolves to the new instance.
	clk = clk.Add(2 * time.Second)
	if tg, _ := r.Resolve("web.apps.local"); tg.GuestIP != "10.20.0.9" {
		t.Errorf("after TTL = %+v, want fresh 10.20.0.9", tg)
	}
}

func TestResolveCacheInstanceGuard(t *testing.T) {
	// Cache app "web" → sbx_1. Then sbx_1 dies and web is re-pointed to sbx_2 at
	// a different IP — WITHIN the TTL. Resolve must return the fresh instance's
	// IP, never the dead one (whose /30 could have been re-leased to another app).
	apps := fakeApps{apps: map[string]api.AppResponse{"web": runningApp("web", 80, "sbx_1")}}
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2"}}
	r := NewResolver(apps, inst, "apps.local", "", time.Second)
	clk := time.Unix(0, 0).UTC()
	r.now = func() time.Time { return clk }

	if tg, _ := r.Resolve("web.apps.local"); tg.GuestIP != "10.20.0.2" {
		t.Fatalf("first resolve = %+v", tg)
	}
	// sbx_1 is gone; web now runs as sbx_2 — still inside the 1s TTL window.
	delete(inst.ips, "sbx_1")
	inst.ips["sbx_2"] = "10.20.0.9"
	apps.apps["web"] = runningApp("web", 80, "sbx_2")

	tg, err := r.Resolve("web.apps.local")
	if err != nil || tg.GuestIP != "10.20.0.9" {
		t.Errorf("within TTL after instance change = %+v, %v; want fresh 10.20.0.9 (no stale/crossover)", tg, err)
	}
}

func TestResolveSetNegatives(t *testing.T) {
	inst := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2"}}
	appOf := func(a api.AppResponse) fakeApps {
		return fakeApps{apps: map[string]api.AppResponse{"web": a}}
	}

	// Unknown host / app → ErrNoRoute.
	r := NewResolver(fakeApps{apps: map[string]api.AppResponse{}}, inst, "apps.local", "", 0)
	if _, err := r.ResolveSet("nope.apps.local"); !errors.Is(err, ErrNoRoute) {
		t.Errorf("unknown → %v, want ErrNoRoute", err)
	}

	// Asleep → ErrAsleep (wakeable, not a hard error).
	asleep := runningApp("web", 80, "sbx_1")
	asleep.Status.Phase = "asleep"
	if _, err := NewResolver(appOf(asleep), inst, "apps.local", "", 0).ResolveSet("web.apps.local"); !errors.Is(err, ErrAsleep) {
		t.Errorf("asleep → %v, want ErrAsleep", err)
	}

	// Running but no live instance IP → ErrNoInstance.
	noIP := runningApp("web", 80, "sbx_missing")
	if _, err := NewResolver(appOf(noIP), inst, "apps.local", "", 0).ResolveSet("web.apps.local"); !errors.Is(err, ErrNoInstance) {
		t.Errorf("no live IP → %v, want ErrNoInstance", err)
	}

	// Running but no target port → ErrNoRoute.
	noPort := runningApp("web", 0, "sbx_1")
	if _, err := NewResolver(appOf(noPort), inst, "apps.local", "", 0).ResolveSet("web.apps.local"); !errors.Is(err, ErrNoRoute) {
		t.Errorf("no port → %v, want ErrNoRoute", err)
	}

	// Multi-instance running app → the whole live endpoint set.
	multi := runningApp("web", 80, "sbx_1")
	multi.Status.Instances = []api.InstanceStatus{{InstanceID: "sbx_1"}, {InstanceID: "sbx_2"}}
	inst2 := fakeInstances{ips: map[string]string{"sbx_1": "10.20.0.2", "sbx_2": "10.20.0.6"}}
	set, err := NewResolver(appOf(multi), inst2, "apps.local", "", 0).ResolveSet("web.apps.local")
	if err != nil || len(set) != 2 {
		t.Errorf("multi-instance set = %v (%v); want 2 targets", set, err)
	}
}
