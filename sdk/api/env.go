package api

import (
	"fmt"
	"strings"
)

// ParseEnv turns docker-style "KEY=VALUE" strings into a map. A key must be
// non-empty and must not contain "="; the value may contain "=" and may be
// empty ("KEY="). On a duplicate key, the later entry wins. An empty input
// returns a nil map (no error).
func ParseEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("env %q: want KEY=VALUE", p)
		}
		out[k] = v
	}
	return out, nil
}
