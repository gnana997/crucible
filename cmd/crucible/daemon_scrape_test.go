//go:build linux

package main

import (
	"testing"

	"github.com/gnana997/crucible/internal/guestscrape"
	"github.com/gnana997/crucible/sdk/api"
)

type fakeApps struct{ apps []api.AppResponse }

func (f fakeApps) List() ([]api.AppResponse, error) { return f.apps, nil }

func TestAppScrapeTargetsFilters(t *testing.T) {
	apps := []api.AppResponse{
		{AppSpec: api.AppSpec{Name: "db", MetricsPort: 9187}, Status: &api.AppStatus{InstanceID: "i1"}},                       // included
		{AppSpec: api.AppSpec{Name: "noport"}, Status: &api.AppStatus{InstanceID: "i2"}},                                      // no metrics port → skip
		{AppSpec: api.AppSpec{Name: "noinstance", MetricsPort: 9121}, Status: &api.AppStatus{}},                               // no live instance → skip
		{AppSpec: api.AppSpec{Name: "nostatus", MetricsPort: 9121}},                                                           // nil status → skip
		{AppSpec: api.AppSpec{Name: "cache", MetricsPort: 9121, MetricsPath: "/m"}, Status: &api.AppStatus{InstanceID: "i5"}}, // included with a path
	}
	got := appScrapeTargets{apps: fakeApps{apps}}.Targets()
	if len(got) != 2 {
		t.Fatalf("targets = %d, want 2: %+v", len(got), got)
	}
	byApp := map[string]guestscrape.Target{}
	for _, tg := range got {
		byApp[tg.App] = tg
	}
	if d := byApp["db"]; d.Port != 9187 || d.Instance != "i1" || d.Path != "" {
		t.Fatalf("db target = %+v", d)
	}
	if c := byApp["cache"]; c.Port != 9121 || c.Instance != "i5" || c.Path != "/m" {
		t.Fatalf("cache target = %+v", c)
	}
}
