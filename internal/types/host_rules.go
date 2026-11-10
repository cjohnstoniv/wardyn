// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// HostRuleSet is one layer's per-host method and path rules (RunPolicySpec.HostRules). A policy's
// host_rules is a LIST of sets because composition never merges rules across layers: first match over
// concatenated rules would let one layer's allow through another layer's no-match deny. Every layer
// keeps its own set, and each set that names a host must allow a request on that host.
type HostRuleSet struct {
	Rules []HostRule `json:"rules"`
}

// HostRule is one rule of a set. Empty Methods matches every method, empty Paths every path. A path
// matches itself and everything below it on a segment boundary ("/a" matches "/a" and "/a/b", never
// "/ab"; "/" matches every path).
type HostRule struct {
	Host    string         `json:"host"`
	Methods []string       `json:"methods,omitempty"`
	Paths   []string       `json:"paths,omitempty"`
	Effect  HostRuleEffect `json:"effect"`
}

// HostRuleEffect is what a matching rule decides.
type HostRuleEffect string

const (
	HostRuleAllow HostRuleEffect = "allow"
	HostRuleDeny  HostRuleEffect = "deny"
	// HostRuleHold is refused by validation until the hold lands (a later release of this train).
	HostRuleHold HostRuleEffect = "hold"
)

// The hostile-input ceilings on host_rules: sets per policy (after composition too) and rules per set.
const (
	MaxHostRuleSets    = 16
	MaxHostRulesPerSet = 64
)

// ValidateHostRuleSets is host_rules' structural check, shared by every door a policy enters through and
// by the composer's overlay check. A rule is authored in normal form: host lowercase and exact, methods
// uppercase tokens, paths as NormalizeRequestPath writes them, so what is stored is what is matched.
func ValidateHostRuleSets(sets []HostRuleSet) error {
	if len(sets) > MaxHostRuleSets {
		return fmt.Errorf("host_rules: at most %d rule sets, got %d", MaxHostRuleSets, len(sets))
	}
	for i, s := range sets {
		if len(s.Rules) == 0 {
			return fmt.Errorf("host_rules[%d].rules: a rule set needs at least one rule", i)
		}
		if len(s.Rules) > MaxHostRulesPerSet {
			return fmt.Errorf("host_rules[%d].rules: at most %d rules per set, got %d", i, MaxHostRulesPerSet, len(s.Rules))
		}
		for j, r := range s.Rules {
			if err := validHostRule(r); err != nil {
				return fmt.Errorf("host_rules[%d].rules[%d].%w", i, j, err)
			}
		}
	}
	return nil
}

func validHostRule(r HostRule) error {
	h := r.Host
	switch {
	case h == "":
		return fmt.Errorf("host: required")
	case h != strings.ToLower(h):
		return fmt.Errorf("host: %q must be lowercase (%q)", h, strings.ToLower(h))
	case strings.ContainsAny(h, "*"):
		return fmt.Errorf("host: %q: a rule names one exact host, never a wildcard", h)
	case strings.ContainsFunc(h, func(c rune) bool { return c <= ' ' || c >= 0x7f || strings.ContainsRune(":/?#[]@\\%", c) }),
		strings.HasSuffix(h, "."), strings.HasPrefix(h, "."):
		return fmt.Errorf("host: %q: a host name or IPv4 address, with no port, path or trailing dot", h)
	}
	for k, m := range r.Methods {
		if m == "" || !isHTTPToken(m) {
			return fmt.Errorf("methods[%d]: %q is no HTTP method token", k, m)
		}
		if m != strings.ToUpper(m) {
			return fmt.Errorf("methods[%d]: %q must be uppercase (%q)", k, m, strings.ToUpper(m))
		}
	}
	for k, p := range r.Paths {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("paths[%d]: %q must start with /", k, p)
		}
		if strings.ContainsAny(p, "?#") {
			return fmt.Errorf("paths[%d]: %q: a rule path carries no query or fragment", k, p)
		}
		n, err := NormalizeRequestPath(p)
		if err != nil {
			return fmt.Errorf("paths[%d]: %w", k, err)
		}
		if n != p {
			return fmt.Errorf("paths[%d]: %q: write it in normal form, %q", k, p, n)
		}
	}
	switch r.Effect {
	case HostRuleAllow, HostRuleDeny:
		return nil
	case HostRuleHold:
		return fmt.Errorf("effect: %q is available in a later release of this train", r.Effect)
	}
	return fmt.Errorf("effect: %q (effects: allow, deny)", r.Effect)
}

// isHTTPToken reports whether s is an RFC 9110 token.
func isHTTPToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

// NormalizeRequestPath is the one path form host rules match: the path without its query or fragment,
// unreserved bytes percent-decoded once (other escapes kept, hex uppercased). Matching is case-sensitive,
// and ";" and what follows it are part of the segment. It refuses what two readers could resolve
// differently: an empty segment ("//"), a "." or ".." segment (encoded, or before a ";"), an encoded "/" or "\",
// a backslash, "%00", any control byte and a malformed escape. raw is the request's escaped path.
func NormalizeRequestPath(raw string) (string, error) {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return "/", nil
	}
	if raw[0] != '/' {
		return "", fmt.Errorf("path %q does not start with /", raw)
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c < 0x20 || c == 0x7f:
			return "", fmt.Errorf("path %q carries a control byte", raw)
		case c == '\\':
			return "", fmt.Errorf("path %q carries a backslash", raw)
		case c >= 0x80:
			fmt.Fprintf(&b, "%%%02X", c)
		case c != '%':
			b.WriteByte(c)
		default:
			if i+2 >= len(raw) || !isHex(raw[i+1]) || !isHex(raw[i+2]) {
				return "", fmt.Errorf("path %q carries a malformed escape", raw)
			}
			v := unhex(raw[i+1])<<4 | unhex(raw[i+2])
			i += 2
			switch {
			case v == 0:
				return "", fmt.Errorf("path %q carries %%00", raw)
			case v == '/' || v == '\\':
				return "", fmt.Errorf("path %q carries an encoded %q", raw, string(rune(v)))
			case v < 0x20 || v == 0x7f:
				return "", fmt.Errorf("path %q carries an encoded control byte", raw)
			case isUnreserved(v):
				b.WriteByte(v)
			default:
				fmt.Fprintf(&b, "%%%02X", v)
			}
		}
	}
	n := b.String()
	segs := strings.Split(n[1:], "/")
	for i, seg := range segs {
		// A layer that strips ";" parameters reads "..;x" as "..", so the part before ";" is checked too.
		base, _, param := strings.Cut(seg, ";")
		if base == "." || base == ".." || base == "" && (param || i < len(segs)-1) {
			return "", fmt.Errorf("path %q is not in normal form: it has an empty, \".\" or \"..\" segment", raw)
		}
	}
	return n, nil
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// HostRuleDecision is the host rules' answer for one request.
type HostRuleDecision struct {
	// Named reports whether any set names the host; false means the rules impose nothing.
	Named bool
	// Effect is allow or deny (hold once holds land).
	Effect HostRuleEffect
	// Set and Rule index the deciding set and rule. Rule is -1 when the set matched no rule (a deny), or
	// when the path could not be read (Set is -1 too).
	Set, Rule int
	// Cause says why a deny that no rule wrote was given.
	Cause string
}

// HostRulesName reports whether any set names host.
func HostRulesName(sets []HostRuleSet, host string) bool {
	return slices.ContainsFunc(sets, func(s HostRuleSet) bool { return s.names(host) })
}

func (s HostRuleSet) names(host string) bool {
	return slices.ContainsFunc(s.Rules, func(r HostRule) bool { return r.Host == host })
}

// EvalHostRules decides a request to host (lowercase, no port). Each set that names the host decides by
// its first matching rule, and a set that names the host but matches no rule denies. Across sets any deny
// wins, then any hold, else allow. A set that does not name the host imposes nothing. rawPath is the
// request's escaped path; one NormalizeRequestPath refuses is denied whenever any set names the host.
func EvalHostRules(sets []HostRuleSet, host, method, rawPath string) HostRuleDecision {
	if !HostRulesName(sets, host) {
		return HostRuleDecision{Effect: HostRuleAllow, Set: -1, Rule: -1}
	}
	path, err := NormalizeRequestPath(rawPath)
	if err != nil {
		return HostRuleDecision{Named: true, Effect: HostRuleDeny, Set: -1, Rule: -1, Cause: err.Error()}
	}
	method = strings.ToUpper(method)
	out := HostRuleDecision{Named: true, Effect: HostRuleAllow, Set: -1, Rule: -1}
	for i, s := range sets {
		if !s.names(host) {
			continue
		}
		eff, j := s.decide(host, method, path)
		if eff == HostRuleDeny {
			cause := ""
			if j < 0 {
				cause = fmt.Sprintf("rule set %d names %s but no rule in it matches %s %s", i, host, method, path)
			}
			return HostRuleDecision{Named: true, Effect: HostRuleDeny, Set: i, Rule: j, Cause: cause}
		}
		if eff == HostRuleHold && out.Effect != HostRuleHold {
			out = HostRuleDecision{Named: true, Effect: HostRuleHold, Set: i, Rule: j}
		}
	}
	return out
}

// decide is one set's first match; no match is a deny with rule -1.
func (s HostRuleSet) decide(host, method, path string) (HostRuleEffect, int) {
	for j, r := range s.Rules {
		if r.Host != host || len(r.Methods) > 0 && !slices.Contains(r.Methods, method) {
			continue
		}
		if len(r.Paths) > 0 && !slices.ContainsFunc(r.Paths, func(p string) bool { return hostRulePathCovers(p, path) }) {
			continue
		}
		return r.Effect, j
	}
	return HostRuleDeny, -1
}

// hostRulePathCovers reports whether rule path p matches request path q on a segment boundary.
func hostRulePathCovers(p, q string) bool {
	return p == "/" || q == p || strings.HasPrefix(q, strings.TrimSuffix(p, "/")+"/")
}

// HostRuleSetKey is a set's normal form as a comparable key: each rule's methods and paths sorted and
// de-duplicated (their order inside a rule means nothing), the rule order kept (first match reads it).
func HostRuleSetKey(s HostRuleSet) string {
	n := HostRuleSet{Rules: make([]HostRule, len(s.Rules))}
	for i, r := range s.Rules {
		r.Methods = slices.Compact(slices.Sorted(slices.Values(r.Methods)))
		r.Paths = slices.Compact(slices.Sorted(slices.Values(r.Paths)))
		n.Rules[i] = r
	}
	b, _ := json.Marshal(n) // strings and slices only: Marshal cannot fail
	return string(b)
}

// AppendHostRuleSets is the one way layers' host rules combine (compose, clamp and the seal): every
// operand's sets appended unchanged, never merged, then de-duplicated and sorted by HostRuleSetKey, so
// the result is the same whichever order the layers come in. No set at all is nil.
func AppendHostRuleSets(lists ...[]HostRuleSet) []HostRuleSet {
	seen := map[string]bool{}
	var out []HostRuleSet
	for _, l := range lists {
		for _, s := range l {
			k := HostRuleSetKey(s)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, CloneHostRuleSet(s))
		}
	}
	slices.SortFunc(out, func(a, b HostRuleSet) int { return strings.Compare(HostRuleSetKey(a), HostRuleSetKey(b)) })
	return out
}

// HostRuleSetsWithin reports whether a is at least as narrow as b: every set of b, in normal form, is
// also a set of a. Extra sets in a only ever refuse more.
func HostRuleSetsWithin(a, b []HostRuleSet) bool {
	have := map[string]bool{}
	for _, s := range a {
		have[HostRuleSetKey(s)] = true
	}
	for _, s := range b {
		if !have[HostRuleSetKey(s)] {
			return false
		}
	}
	return true
}

// CloneHostRuleSet deep-copies one set.
func CloneHostRuleSet(s HostRuleSet) HostRuleSet {
	out := HostRuleSet{Rules: slices.Clone(s.Rules)}
	for i := range out.Rules {
		out.Rules[i].Methods = slices.Clone(s.Rules[i].Methods)
		out.Rules[i].Paths = slices.Clone(s.Rules[i].Paths)
	}
	return out
}

// CloneHostRuleSets deep-copies a list of sets, keeping nil as nil.
func CloneHostRuleSets(sets []HostRuleSet) []HostRuleSet {
	if sets == nil {
		return nil
	}
	out := make([]HostRuleSet, len(sets))
	for i, s := range sets {
		out[i] = CloneHostRuleSet(s)
	}
	return out
}
