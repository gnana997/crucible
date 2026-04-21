package network

import (
	"strings"
	"testing"
)

func TestNewAcceptsValidPatterns(t *testing.T) {
	patterns := []string{
		"pypi.org",
		"*.npmjs.org",
		"registry.npmjs.org",
		"objects.githubusercontent.com",
		"proxy.golang.org",
	}
	a, err := New(patterns)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := len(a.Patterns()); got != len(patterns) {
		t.Errorf("Patterns length = %d, want %d", got, len(patterns))
	}
}

func TestNewRejectsBadPatterns(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		wantSub string
	}{
		{"bare wildcard", "*", "bare wildcard"},
		{"multi wildcard", "*.*.foo.com", "first label"},
		{"inner wildcard", "foo.*.com", "first label"},
		{"embedded wildcard", "a*b.com", "invalid character"},
		{"empty", "", "empty pattern"},
		{"whitespace", "   ", "empty pattern"},
		{"single label", "localhost", "at least two labels"},
		{"leading hyphen", "-bad.com", "must not start or end with '-'"},
		{"trailing hyphen", "bad-.com", "must not start or end with '-'"},
		{"overlong label", strings.Repeat("a", 64) + ".com", "exceeds DNS limit"},
		{"overlong total", strings.Repeat("a.", 200) + "com", "exceeds DNS limit"},
		{"underscore", "foo_bar.com", "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New([]string{tc.pattern})
			if err == nil {
				t.Fatalf("expected error for %q", tc.pattern)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestNewNormalizesCaseAndTrailingDot(t *testing.T) {
	a, err := New([]string{"PyPI.ORG.", "  pypi.org  "})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pats := a.Patterns()
	// Duplicates should collapse into one canonical entry.
	if len(pats) != 1 {
		t.Fatalf("Patterns = %v, want 1 entry after dedup", pats)
	}
	if pats[0] != "pypi.org" {
		t.Errorf("canonical = %q, want %q", pats[0], "pypi.org")
	}
}

func TestMatchesExact(t *testing.T) {
	a, err := New([]string{"pypi.org"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"pypi.org":      true,
		"PYPI.ORG":      true,
		"pypi.org.":     true,
		"Pypi.Org.":     true,
		"sub.pypi.org":  false,
		"notpypi.org":   false,
		"pypi.org.evil": false,
		"pypi":          false,
		"":              false,
	}
	for name, want := range cases {
		if got := a.Matches(name); got != want {
			t.Errorf("Matches(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMatchesWildcard(t *testing.T) {
	a, err := New([]string{"*.npmjs.org"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"registry.npmjs.org":  true,
		"www.npmjs.org":       true,
		"REGISTRY.npmjs.org":  true,
		"registry.npmjs.org.": true,

		// single-label wildcard must not match multi-level subdomains
		"a.b.npmjs.org": false,
		"x.y.z.npmjs.org": false,

		// must not match the bare apex
		"npmjs.org": false,

		// must not match similar-but-unrelated names
		"npmjs.org.evil.com": false,
		"fake-npmjs.org":     false,
		"registry.npmjs.com": false,
	}
	for name, want := range cases {
		if got := a.Matches(name); got != want {
			t.Errorf("Matches(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMatchesBothExactAndWildcard(t *testing.T) {
	a, err := New([]string{"npmjs.org", "*.npmjs.org"})
	if err != nil {
		t.Fatal(err)
	}
	// Apex matches the exact entry, subdomain matches the wildcard.
	// Both should return true — trie must accommodate co-existing
	// exact + wildcard at the same depth.
	for name, want := range map[string]bool{
		"npmjs.org":          true,
		"registry.npmjs.org": true,
		"a.b.npmjs.org":      false, // wildcard is single-label
	} {
		if got := a.Matches(name); got != want {
			t.Errorf("Matches(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMatchesMultipleTLDs(t *testing.T) {
	// Sanity: patterns under different TLDs don't leak across in
	// the reversed-label trie.
	a, err := New([]string{"*.npmjs.org", "pypi.org", "*.pypi.io"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"registry.npmjs.org": true,
		"pypi.org":           true,
		"files.pypi.io":      true,

		"pypi.npmjs.org": true,  // matches *.npmjs.org
		"npmjs.pypi.org": false, // pypi.org is exact-only
		"npmjs.org":      false, // bare — no exact entry
	}
	for name, want := range cases {
		if got := a.Matches(name); got != want {
			t.Errorf("Matches(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMatchesRejectsAdversarialQueries(t *testing.T) {
	a, err := New([]string{"pypi.org"})
	if err != nil {
		t.Fatal(err)
	}
	// These are all malformed or pathological. Matches should
	// return false without panicking; we intentionally do NOT
	// enforce pattern-level validation on the query side.
	bad := []string{
		"",
		".",
		"..",
		"....",
		strings.Repeat("a", 300),
		strings.Repeat("a.", 200),
		"foo..bar",
		"foo bar.com",
	}
	for _, q := range bad {
		// Must not panic. The assertion is just "doesn't crash";
		// whether it matches or not is less interesting than
		// robustness.
		_ = a.Matches(q)
	}
}

func TestMatchesZeroAllowlist(t *testing.T) {
	a, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pypi.org", "", "anything.com"} {
		if a.Matches(name) {
			t.Errorf("empty allowlist matched %q", name)
		}
	}

	// Nil receiver must also be safe.
	var nilA *Allowlist
	if nilA.Matches("pypi.org") {
		t.Error("nil Allowlist matched")
	}
}

func TestPatternsPreservesInsertionOrder(t *testing.T) {
	a, err := New([]string{"c.com", "a.com", "b.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c.com", "a.com", "b.com"}
	got := a.Patterns()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Patterns()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
