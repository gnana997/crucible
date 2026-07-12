package telemetry

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestNewResourceDefaults(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	r := NewResource("")
	if r.ServiceName != "crucible" {
		t.Errorf("ServiceName = %q, want crucible", r.ServiceName)
	}
	if r.ServiceVersion == "" {
		t.Error("ServiceVersion empty")
	}
	if r.HostName == "" {
		t.Error("HostName empty")
	}
	if r.Extra != nil {
		t.Errorf("Extra = %v, want nil", r.Extra)
	}
}

func TestNewResourceEnvAndFlag(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "from-env")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "region=blr, env=prod ,bad,=noKey,onlyKey=")

	// explicit flag wins over env
	if got := NewResource("from-flag").ServiceName; got != "from-flag" {
		t.Errorf("ServiceName = %q, want from-flag (flag overrides env)", got)
	}
	// env used when flag empty
	r := NewResource("")
	if r.ServiceName != "from-env" {
		t.Errorf("ServiceName = %q, want from-env", r.ServiceName)
	}
	if r.Extra["region"] != "blr" || r.Extra["env"] != "prod" {
		t.Errorf("Extra = %v, want region=blr env=prod", r.Extra)
	}
	if _, ok := r.Extra["bad"]; ok {
		t.Error("bare token 'bad' should be dropped (no '=')")
	}
	if _, ok := r.Extra[""]; ok {
		t.Error("empty key should be dropped")
	}
	if v, ok := r.Extra["onlyKey"]; !ok || v != "" {
		t.Errorf("onlyKey should map to empty value, got %q ok=%v", v, ok)
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{100: "1xx", 204: "2xx", 301: "3xx", 404: "4xx", 500: "5xx", 599: "5xx", 0: "1xx"}
	for code, want := range cases {
		if got := StatusClass(code); got != want {
			t.Errorf("StatusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	loop := []string{"127.0.0.1:6060", "localhost:6060", "[::1]:6060"}
	notLoop := []string{":6060", "0.0.0.0:6060", "192.168.1.5:6060"}
	for _, a := range loop {
		if !IsLoopbackAddr(a) {
			t.Errorf("IsLoopbackAddr(%q) = false, want true", a)
		}
	}
	for _, a := range notLoop {
		if IsLoopbackAddr(a) {
			t.Errorf("IsLoopbackAddr(%q) = true, want false", a)
		}
	}
}

func TestPprofMuxServes(t *testing.T) {
	mux := pprofMux()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/goroutine"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// fakeExporter records shutdown for the Provider lifecycle test.
type fakeExporter struct {
	name    string
	downErr error
	down    bool
}

func (f *fakeExporter) Name() string                   { return f.name }
func (f *fakeExporter) Shutdown(context.Context) error { f.down = true; return f.downErr }

func TestProviderLifecycle(t *testing.T) {
	// nil-safe
	var nilP *Provider
	if err := nilP.Shutdown(context.Background()); err != nil {
		t.Errorf("nil Provider Shutdown = %v", err)
	}
	nilP.Register(&fakeExporter{name: "x"}) // must not panic

	p := New(context.Background(), Config{ServiceName: "test"})
	if p.Resource.ServiceName != "test" {
		t.Errorf("resource service name = %q", p.Resource.ServiceName)
	}
	a, b := &fakeExporter{name: "a"}, &fakeExporter{name: "b"}
	p.Register(a)
	p.Register(b)
	p.Register(nil) // ignored
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if !a.down || !b.down {
		t.Errorf("exporters not shut down: a=%v b=%v", a.down, b.down)
	}
}
