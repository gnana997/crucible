package network

import (
	"errors"
	"fmt"
	"strings"
)

// Allowlist is an immutable matcher over a set of hostname patterns.
//
// Patterns are stored as a trie keyed by *reversed* DNS labels: the
// pattern `*.npmjs.org` becomes the label sequence [org, npmjs, *]
// and is inserted at depth 3 of the trie. Queries walk the trie
// label-by-label in the same reversed direction, branching through
// exact children first and wildcards second.
//
// Reversing labels lets us share tries across unrelated TLDs
// without losing O(labels) lookup, and it makes the wildcard
// semantics easy to reason about: a single-label wildcard
// (`*.npmjs.org`) is a node that matches exactly one level at its
// position and must be followed by a terminal.
//
// The struct is safe for concurrent Matches calls from many
// goroutines — it is fully constructed at New time and never
// mutated afterward.
type Allowlist struct {
	root      *node
	canonical []string // normalized, dedup'd patterns in insertion order
}

// node is one level of the reversed-label trie.
//
// children maps the next label (e.g. "npmjs") to its subtree.
// wildcard, if non-nil, is the subtree to follow when the query's
// next label is anything. terminal=true means a pattern ends here
// — a match is only a match if the query has exhausted its labels
// at a terminal node.
type node struct {
	children map[string]*node
	wildcard *node
	terminal bool
}

// New parses patterns and returns an Allowlist. On the first
// invalid pattern it returns an error; the Allowlist is nil in
// that case.
//
// Accepted forms:
//
//   - Exact hostname: pypi.org
//   - Single-label wildcard at the most-specific position only:
//     *.npmjs.org
//
// Normalization: lowercased, trailing dot stripped, whitespace
// around the whole pattern trimmed.
//
// Rejected (with a clear error each):
//
//   - Bare "*" (too broad; not a reasonable allowlist entry)
//   - Wildcards anywhere but the first label: *.*.foo.com, foo.*.com
//   - Labels that aren't syntactically valid DNS labels
//   - Total length > 253 chars or any label > 63 chars (DNS limits)
//   - Empty strings or whitespace-only strings
func New(patterns []string) (*Allowlist, error) {
	a := &Allowlist{root: newNode()}
	seen := make(map[string]bool, len(patterns))
	for _, raw := range patterns {
		p, err := normalizePattern(raw)
		if err != nil {
			return nil, fmt.Errorf("allowlist: pattern %q: %w", raw, err)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		a.canonical = append(a.canonical, p)
		if err := a.insert(p); err != nil {
			return nil, fmt.Errorf("allowlist: pattern %q: %w", raw, err)
		}
	}
	return a, nil
}

// Matches reports whether name is allowed under this Allowlist.
// name is normalized the same way patterns are (lowercase,
// trailing dot stripped) before matching, so callers can pass raw
// DNS query names without pre-processing.
//
// Zero allowlists (no patterns) never match.
func (a *Allowlist) Matches(name string) bool {
	if a == nil || a.root == nil {
		return false
	}
	name, err := normalizeQuery(name)
	if err != nil {
		return false
	}
	labels := reversedLabels(name)
	return matchWalk(a.root, labels, 0)
}

// Patterns returns the normalized, de-duplicated patterns in the
// order they were provided. Useful for echoing the applied policy
// back to the client in sandboxResponse.
func (a *Allowlist) Patterns() []string {
	if a == nil {
		return nil
	}
	out := make([]string, len(a.canonical))
	copy(out, a.canonical)
	return out
}

// --- trie ops ---------------------------------------------------

func newNode() *node {
	return &node{children: map[string]*node{}}
}

// insert walks the trie along the reversed labels of pattern and
// marks the final node terminal. A wildcard label creates (or
// reuses) the wildcard branch at its level.
func (a *Allowlist) insert(pattern string) error {
	labels := reversedLabels(pattern)
	cur := a.root
	for i, l := range labels {
		if l == "*" {
			// Wildcard is only valid as the final (reversed)
			// label, i.e. the first (original) label. In the
			// reversed iteration this means i == len(labels)-1.
			if i != len(labels)-1 {
				return errors.New("wildcard allowed only as the first label (e.g. *.npmjs.org)")
			}
			if cur.wildcard == nil {
				cur.wildcard = newNode()
			}
			cur.wildcard.terminal = true
			return nil
		}
		next, ok := cur.children[l]
		if !ok {
			next = newNode()
			cur.children[l] = next
		}
		cur = next
	}
	cur.terminal = true
	return nil
}

// matchWalk does the read-side trie walk. It prefers exact
// children over wildcards because an entry in the allowlist with
// both `npmjs.org` and `*.npmjs.org` should still work for every
// query.
func matchWalk(cur *node, labels []string, i int) bool {
	if cur == nil {
		return false
	}
	if i == len(labels) {
		return cur.terminal
	}
	// Exact match on this label.
	if next, ok := cur.children[labels[i]]; ok {
		if matchWalk(next, labels, i+1) {
			return true
		}
	}
	// Wildcard match: consumes exactly this one label and must
	// be the terminal label (single-label wildcard semantics).
	if cur.wildcard != nil && i == len(labels)-1 {
		return cur.wildcard.terminal
	}
	return false
}

// --- normalization + validation --------------------------------

const (
	maxNameLength  = 253
	maxLabelLength = 63
)

// normalizePattern produces the canonical form of an
// allowlist-side pattern or returns an error describing why it's
// invalid. Pattern-side rules are strict: wildcards allowed only
// at the first label, labels must parse as DNS labels.
func normalizePattern(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".")
	s = strings.ToLower(s)
	if s == "" {
		return "", errors.New("empty pattern")
	}
	if s == "*" {
		return "", errors.New("bare wildcard '*' not allowed — allowlists must be specific")
	}
	if len(s) > maxNameLength {
		return "", fmt.Errorf("total length %d exceeds DNS limit %d", len(s), maxNameLength)
	}
	labels := strings.Split(s, ".")
	for i, l := range labels {
		if l == "*" {
			// Wildcard must be at the first label (the leftmost
			// in the original, which is index 0 here).
			if i != 0 {
				return "", errors.New("wildcard '*' allowed only as the first label")
			}
			continue
		}
		if err := validateLabel(l); err != nil {
			return "", fmt.Errorf("label %q: %w", l, err)
		}
	}
	if len(labels) < 2 {
		return "", errors.New("pattern must have at least two labels (e.g. example.com)")
	}
	return s, nil
}

// normalizeQuery is the query-side equivalent: lighter than
// pattern validation because we don't reject queries with weird
// shapes — we simply won't match them.
func normalizeQuery(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".")
	s = strings.ToLower(s)
	if s == "" {
		return "", errors.New("empty name")
	}
	if len(s) > maxNameLength {
		return "", errors.New("name too long")
	}
	return s, nil
}

// validateLabel enforces RFC 1035-ish label rules: length 1..63,
// composed of ASCII letters/digits/hyphens, must not start or end
// with a hyphen. Underscores are technically forbidden in
// hostnames but are common in service records; we're strict here
// because these are user-supplied patterns, not received queries.
func validateLabel(l string) error {
	if l == "" {
		return errors.New("empty label")
	}
	if len(l) > maxLabelLength {
		return fmt.Errorf("label length %d exceeds DNS limit %d", len(l), maxLabelLength)
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return errors.New("label must not start or end with '-'")
	}
	for _, r := range l {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-':
			// ok
		default:
			return fmt.Errorf("invalid character %q in label", r)
		}
	}
	return nil
}

// reversedLabels returns the DNS labels of name in reverse order:
// "registry.npmjs.org" → ["org", "npmjs", "registry"].
func reversedLabels(name string) []string {
	parts := strings.Split(name, ".")
	out := make([]string, len(parts))
	for i := range parts {
		out[len(parts)-1-i] = parts[i]
	}
	return out
}
