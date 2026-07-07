package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

func ptr(s []string) *[]string { return &s }

// --- Parse ------------------------------------------------------------------

func TestParseFullRoundTrip(t *testing.T) {
	in := `{
	  "operations": ["create","exec","delete"],
	  "net_allow_max": ["pypi.org","*.npmjs.org"],
	  "allow_profiles": ["python-3.12"],
	  "max_sandboxes": 8, "max_fork": 4, "max_timeout_s": 300,
	  "max_vcpus": 2, "max_memory_mib": 1024
	}`
	p, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Operations) != 3 || p.Operations[0] != OpCreate {
		t.Errorf("operations = %v", p.Operations)
	}
	if p.NetAllowMax == nil || len(*p.NetAllowMax) != 2 {
		t.Errorf("net_allow_max = %v", p.NetAllowMax)
	}
	if p.MaxSandboxes != 8 || p.MaxFork != 4 || p.MaxTimeoutS != 300 || p.MaxVCPUs != 2 || p.MaxMemoryMiB != 1024 {
		t.Errorf("caps = %+v", p)
	}

	// Marshaling back preserves the fields (and omits the zero ones).
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var p2 Policy
	if err := json.Unmarshal(out, &p2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if p2.MaxSandboxes != 8 || p2.NetAllowMax == nil {
		t.Errorf("round-trip lost data: %+v", p2)
	}
}

func TestParseEmptyIsPermissive(t *testing.T) {
	p, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("empty policy should be valid: %v", err)
	}
	if !p.Allows(OpFork) || p.NetAllowMax != nil || len(p.AllowProfiles) != 0 {
		t.Errorf("empty policy should be fully permissive: %+v", p)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := Parse([]byte(`{"max_sandboxes": 8, "max_snadboxes": 9}`)) // typo
	if err == nil {
		t.Fatal("expected error on unknown field")
	}
	if !strings.Contains(err.Error(), "max_snadboxes") {
		t.Errorf("error should name the unknown field: %v", err)
	}
}

func TestParseRejectsMalformedAndTrailing(t *testing.T) {
	if _, err := Parse([]byte(`{`)); err == nil {
		t.Error("expected error on malformed JSON")
	}
	if _, err := Parse([]byte(`{} garbage`)); err == nil {
		t.Error("expected error on trailing content")
	}
}

func TestParseNetAllowMaxTriState(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantNil    bool
		wantLen    int
		wantNonNil bool
	}{
		{"absent → nil", `{}`, true, 0, false},
		{"null → nil", `{"net_allow_max": null}`, true, 0, false},
		{"empty → deny-all (non-nil, len 0)", `{"net_allow_max": []}`, false, 0, true},
		{"list → subset", `{"net_allow_max": ["pypi.org"]}`, false, 1, true},
	}
	for _, c := range cases {
		p, err := Parse([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.wantNil && p.NetAllowMax != nil {
			t.Errorf("%s: want nil, got %v", c.name, *p.NetAllowMax)
		}
		if c.wantNonNil {
			if p.NetAllowMax == nil {
				t.Errorf("%s: want non-nil pointer", c.name)
			} else if len(*p.NetAllowMax) != c.wantLen {
				t.Errorf("%s: len = %d, want %d", c.name, len(*p.NetAllowMax), c.wantLen)
			}
		}
	}
}

// --- Validate ---------------------------------------------------------------

func TestValidateValid(t *testing.T) {
	valid := []Policy{
		{},                              // permissive
		{Operations: KnownOperations()}, // all known ops
		{NetAllowMax: ptr(nil)},         // deny-all network (nil slice via pointer)
		{NetAllowMax: ptr([]string{})},  // deny-all network (empty)
		{NetAllowMax: ptr([]string{"pypi.org", "*.npmjs.org"})},
		{AllowProfiles: []string{"base", "python-3.12"}},
		{MaxSandboxes: 8, MaxFork: 4, MaxTimeoutS: 300, MaxVCPUs: 2, MaxMemoryMiB: 512},
	}
	for i, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("case %d should be valid: %v", i, err)
		}
	}
}

func TestValidateUnknownOperation(t *testing.T) {
	err := Policy{Operations: []Operation{OpCreate, "frobnicate"}}.Validate()
	if err == nil {
		t.Fatal("expected error on unknown operation")
	}
	if !strings.Contains(err.Error(), "frobnicate") || !strings.Contains(err.Error(), "create") {
		t.Errorf("error should name the bad op and list valid ones: %v", err)
	}
}

func TestValidateBadNetPattern(t *testing.T) {
	for _, bad := range [][]string{
		{"*"},         // bare wildcard
		{"foo.*.com"}, // wildcard not first label
		{""},          // empty
		{"pypi.org", "*"},
	} {
		if err := (Policy{NetAllowMax: ptr(bad)}).Validate(); err == nil {
			t.Errorf("net_allow_max %v should be invalid", bad)
		}
	}
}

func TestValidateEmptyNetIsValid(t *testing.T) {
	// deny-all-network is a legitimate, common policy — not an error.
	if err := (Policy{NetAllowMax: ptr([]string{})}).Validate(); err != nil {
		t.Errorf("empty net_allow_max (deny all) should be valid: %v", err)
	}
}

func TestValidateEmptyProfileEntry(t *testing.T) {
	if err := (Policy{AllowProfiles: []string{"base", "  "}}).Validate(); err == nil {
		t.Error("blank profile entry should be invalid")
	}
}

func TestValidateNegativeCaps(t *testing.T) {
	for _, p := range []Policy{
		{MaxSandboxes: -1}, {MaxFork: -1}, {MaxTimeoutS: -1}, {MaxVCPUs: -1}, {MaxMemoryMiB: -1},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("negative cap should be invalid: %+v", p)
		}
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	// Every problem at once — the joined error should mention each.
	err := Policy{
		Operations:   []Operation{"nope"},
		NetAllowMax:  ptr([]string{"*"}),
		MaxSandboxes: -5,
	}.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"nope", "net_allow_max", "max_sandboxes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q: %v", want, err)
		}
	}
}

func TestParseAndValidate(t *testing.T) {
	if _, err := ParseAndValidate([]byte(`{"operations":["create"]}`)); err != nil {
		t.Errorf("valid: %v", err)
	}
	if _, err := ParseAndValidate([]byte(`{"bogus":1}`)); err == nil {
		t.Error("unknown field should fail")
	}
	if _, err := ParseAndValidate([]byte(`{"operations":["nope"]}`)); err == nil {
		t.Error("unknown op should fail")
	}
}

// --- enforcement ------------------------------------------------------------

func TestAllows(t *testing.T) {
	if !(Policy{}).Allows(OpFork) {
		t.Error("empty policy should allow all operations")
	}
	p := Policy{Operations: []Operation{OpCreate, OpExec}}
	if !p.Allows(OpCreate) || !p.Allows(OpExec) {
		t.Error("listed ops should be allowed")
	}
	for _, op := range []Operation{OpFork, OpSnapshot, OpDelete, OpRead} {
		if p.Allows(op) {
			t.Errorf("op %q should not be allowed", op)
		}
	}
}

func TestCheckProfile(t *testing.T) {
	none := Policy{}
	if err := none.CheckProfile("anything"); err != nil {
		t.Errorf("no allowlist should allow any profile: %v", err)
	}
	if err := none.CheckProfile(""); err != nil {
		t.Errorf("no allowlist should allow the default profile: %v", err)
	}
	p := Policy{AllowProfiles: []string{"base", "python-3.12"}}
	if err := p.CheckProfile("base"); err != nil {
		t.Errorf("allowed profile rejected: %v", err)
	}
	if err := p.CheckProfile("ghost"); err == nil {
		t.Error("disallowed profile should be rejected")
	}
	if err := p.CheckProfile(""); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("empty profile under an allowlist should be rejected as required: %v", err)
	}
}

func TestCheckNetAllow(t *testing.T) {
	// nil ceiling → anything, including a network request.
	if err := (Policy{}).CheckNetAllow([]string{"anything.com"}); err != nil {
		t.Errorf("nil ceiling should permit any host: %v", err)
	}
	// empty ceiling → deny all network.
	deny := Policy{NetAllowMax: ptr([]string{})}
	if err := deny.CheckNetAllow(nil); err != nil {
		t.Errorf("empty request under deny-all should pass: %v", err)
	}
	if err := deny.CheckNetAllow([]string{"pypi.org"}); err == nil {
		t.Error("deny-all ceiling should reject any host")
	}
	// subset ceiling.
	ceil := Policy{NetAllowMax: ptr([]string{"pypi.org", "*.npmjs.org"})}
	if err := ceil.CheckNetAllow([]string{"pypi.org"}); err != nil {
		t.Errorf("subset should pass: %v", err)
	}
	if err := ceil.CheckNetAllow(nil); err != nil {
		t.Errorf("empty request is a subset: %v", err)
	}
	if err := ceil.CheckNetAllow([]string{"pypi.org", "evil.com"}); err == nil {
		t.Error("out-of-ceiling host should be rejected")
	}
	// normalization: case + trailing dot must not matter.
	if err := ceil.CheckNetAllow([]string{"PyPI.org."}); err != nil {
		t.Errorf("normalized match should pass: %v", err)
	}
}

func TestClampTimeout(t *testing.T) {
	if got := (Policy{}).ClampTimeout(0); got != 0 {
		t.Errorf("no clamp: got %d, want 0", got)
	}
	if got := (Policy{}).ClampTimeout(999); got != 999 {
		t.Errorf("no clamp: got %d, want 999", got)
	}
	p := Policy{MaxTimeoutS: 300}
	for _, tc := range []struct{ in, want int }{{0, 300}, {100, 100}, {300, 300}, {500, 300}} {
		if got := p.ClampTimeout(tc.in); got != tc.want {
			t.Errorf("ClampTimeout(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCheckFork(t *testing.T) {
	if err := (Policy{}).CheckFork(1000); err != nil {
		t.Errorf("no cap should allow any count: %v", err)
	}
	p := Policy{MaxFork: 8}
	if err := p.CheckFork(8); err != nil {
		t.Errorf("count at the cap should pass: %v", err)
	}
	if err := p.CheckFork(9); err == nil {
		t.Error("count over the cap should be rejected")
	}
}

func TestCheckCapacity(t *testing.T) {
	if err := (Policy{}).CheckCapacity(1000, 1000); err != nil {
		t.Errorf("no cap should allow anything: %v", err)
	}
	p := Policy{MaxSandboxes: 8}
	cases := []struct {
		live, want int
		ok         bool
	}{
		{7, 1, true},  // → 8, at the cap
		{8, 1, false}, // → 9
		{7, 2, false}, // → 9
		{0, 8, true},  // exactly the cap
		{0, 9, false},
	}
	for _, c := range cases {
		err := p.CheckCapacity(c.live, c.want)
		if (err == nil) != c.ok {
			t.Errorf("CheckCapacity(live=%d, want=%d): err=%v, wantOK=%v", c.live, c.want, err, c.ok)
		}
	}
}

func TestCheckVCPUsAndMemory(t *testing.T) {
	if err := (Policy{}).CheckVCPUs(64); err != nil {
		t.Errorf("no vcpu cap: %v", err)
	}
	if err := (Policy{}).CheckMemory(1 << 20); err != nil {
		t.Errorf("no mem cap: %v", err)
	}
	pv := Policy{MaxVCPUs: 4}
	if err := pv.CheckVCPUs(4); err != nil {
		t.Errorf("vcpus at cap should pass: %v", err)
	}
	if err := pv.CheckVCPUs(5); err == nil {
		t.Error("vcpus over cap should be rejected")
	}
	if err := pv.CheckVCPUs(0); err != nil {
		t.Errorf("vcpus 0 (daemon default) should pass: %v", err)
	}
	pm := Policy{MaxMemoryMiB: 512}
	if err := pm.CheckMemory(512); err != nil {
		t.Errorf("mem at cap should pass: %v", err)
	}
	if err := pm.CheckMemory(513); err == nil {
		t.Error("mem over cap should be rejected")
	}
	if err := pm.CheckMemory(0); err != nil {
		t.Errorf("mem 0 (daemon default) should pass: %v", err)
	}
}
