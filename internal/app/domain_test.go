package app

import (
	"testing"

	"github.com/gnana997/crucible/sdk/wire"
)

func TestDomainAddListRemove(t *testing.T) {
	m, _ := newMgr(t, newFake())
	if _, err := m.Create(nginxSpec("web", wire.RestartAlways), true); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Pre-warm hook fires with the attached domain.
	var warmed []string
	m.SetOnDomainAdd(func(d string) { warmed = append(warmed, d) })

	if _, err := m.AddDomain("web", "Shop.ACME.com"); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	// Stored lowercased; idempotent re-add is a no-op.
	if _, err := m.AddDomain("web", "shop.acme.com"); err != nil {
		t.Fatalf("AddDomain (idempotent): %v", err)
	}
	got, err := m.ListDomains("web")
	if err != nil || len(got) != 1 || got[0] != "shop.acme.com" {
		t.Fatalf("ListDomains = %v, %v; want [shop.acme.com]", got, err)
	}
	if len(warmed) != 1 || warmed[0] != "shop.acme.com" {
		t.Errorf("pre-warm hook fired %v, want [shop.acme.com] once", warmed)
	}

	// Reverse lookup finds the app.
	if resp, ok := m.GetByDomain("shop.acme.com"); !ok || resp.Name != "web" {
		t.Errorf("GetByDomain = %v, %v; want web", resp.Name, ok)
	}

	if _, err := m.RemoveDomain("web", "shop.acme.com"); err != nil {
		t.Fatalf("RemoveDomain: %v", err)
	}
	if got, _ := m.ListDomains("web"); len(got) != 0 {
		t.Errorf("after remove, domains = %v, want empty", got)
	}
}

func TestDomainGlobalUniqueness(t *testing.T) {
	m, _ := newMgr(t, newFake())
	if _, err := m.Create(nginxSpec("web", wire.RestartAlways), true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(nginxSpec("api", wire.RestartAlways), true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDomain("web", "shop.acme.com"); err != nil {
		t.Fatal(err)
	}
	// The same domain cannot attach to a second app.
	if _, err := m.AddDomain("api", "shop.acme.com"); err == nil {
		t.Fatal("AddDomain to a second app should fail (global uniqueness)")
	}
}

func TestDomainInvalid(t *testing.T) {
	m, _ := newMgr(t, newFake())
	if _, err := m.Create(nginxSpec("web", wire.RestartAlways), true); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "notadomain", "*.wildcard.com", "no spaces.com"} {
		if _, err := m.AddDomain("web", bad); err == nil {
			t.Errorf("AddDomain(%q) should be rejected", bad)
		}
	}
}

func TestUpdatePreservesDomains(t *testing.T) {
	m, _ := newMgr(t, newFake())
	if _, err := m.Create(nginxSpec("web", wire.RestartAlways), true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDomain("web", "shop.acme.com"); err != nil {
		t.Fatal(err)
	}
	// An `app update` that doesn't carry Domains must not wipe them.
	spec := nginxSpec("web", wire.RestartAlways)
	spec.MemoryMiB = 512 // some unrelated change
	if _, err := m.Update("web", spec); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := m.ListDomains("web"); len(got) != 1 || got[0] != "shop.acme.com" {
		t.Errorf("domains after update = %v, want preserved [shop.acme.com]", got)
	}
}
