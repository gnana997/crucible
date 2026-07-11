package api

import "testing"

func TestParseEnv(t *testing.T) {
	t.Run("nil on empty", func(t *testing.T) {
		m, err := ParseEnv(nil)
		if err != nil || m != nil {
			t.Fatalf("ParseEnv(nil) = %v, %v; want nil, nil", m, err)
		}
	})

	t.Run("parses pairs, later wins, empty value ok, value may hold =", func(t *testing.T) {
		m, err := ParseEnv([]string{"A=1", "B=", "C=x=y", "A=2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]string{"A": "2", "B": "", "C": "x=y"}
		if len(m) != len(want) {
			t.Fatalf("got %v, want %v", m, want)
		}
		for k, v := range want {
			if m[k] != v {
				t.Errorf("key %q = %q, want %q", k, m[k], v)
			}
		}
	})

	for _, bad := range []string{"NOEQUALS", "=novalue", ""} {
		if _, err := ParseEnv([]string{bad}); err == nil {
			t.Errorf("ParseEnv([%q]) = nil error; want a KEY=VALUE error", bad)
		}
	}
}
