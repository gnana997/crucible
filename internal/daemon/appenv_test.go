package daemon

import (
	"testing"

	"github.com/gnana997/crucible/sdk/wire"
)

func TestMergeAppEnv(t *testing.T) {
	t.Run("app env overlays image env, app wins", func(t *testing.T) {
		svc := &wire.ServiceSpec{Env: map[string]string{
			"NGINX_VERSION": "1.31.2",
			"PATH":          "/usr/bin",
		}}
		mergeAppEnv(svc, map[string]string{
			"CRUCIBLE_ENV_PROBE": "marker-xyz",
			"PATH":               "/override", // app wins over image
		})
		if svc.Env["CRUCIBLE_ENV_PROBE"] != "marker-xyz" {
			t.Errorf("app env not applied: %v", svc.Env)
		}
		if svc.Env["NGINX_VERSION"] != "1.31.2" {
			t.Errorf("image env dropped: %v", svc.Env)
		}
		if svc.Env["PATH"] != "/override" {
			t.Errorf("app env should win over image env: %v", svc.Env)
		}
	})

	t.Run("no-op cases don't panic", func(t *testing.T) {
		mergeAppEnv(nil, map[string]string{"A": "1"}) // nil service: nowhere to land
		svc := &wire.ServiceSpec{Env: map[string]string{"A": "1"}}
		mergeAppEnv(svc, nil) // no app env
		if svc.Env["A"] != "1" {
			t.Errorf("nil app env should leave service env untouched: %v", svc.Env)
		}
	})

	t.Run("applies onto a service with no env", func(t *testing.T) {
		svc := &wire.ServiceSpec{}
		mergeAppEnv(svc, map[string]string{"K": "v"})
		if svc.Env["K"] != "v" {
			t.Errorf("env not set on an empty-env service: %v", svc.Env)
		}
	})
}
