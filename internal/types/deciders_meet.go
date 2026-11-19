// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "slices"

// The meet of the rules for one kind along a unit's chain: a unit's rule only narrows the rules above it.
// A field a rule gains extends both MeetDeciderRules and DeciderRuleWithin, each with its own narrowest reading.

// MeetDeciderRules is what a chain of rules for one kind leaves, nearest first. The result:
//   - who: the entries every rule names, in the nearest rule's order; none left means the security
//     tier alone, as an empty who does;
//   - second_human: set when any rule sets it;
//   - max_scope: the narrowest named (an empty max_scope reaches as far as run);
//   - max_elevation: the shortest named;
//   - eligible_requesters: the groups every rule names, in the nearest rule's order (none left offers
//     nobody); renew_after: the longest named. ticket_pattern is not met and stays empty: every pattern
//     on the chain binds a ticket, and one string cannot hold several (the request reads the chain).
//
// Kinds is [kind]. An empty chain is no rule (false).
func MeetDeciderRules(kind string, chain []DeciderRule) (DeciderRule, bool) {
	if len(chain) == 0 {
		return DeciderRule{}, false
	}
	m := DeciderRule{Kinds: []string{kind}, Who: append([]string{}, chain[0].Who...)}
	if kind == DeciderKindElevation {
		m.EligibleRequesters = slices.Clone(chain[0].EligibleRequesters)
	}
	for _, r := range chain {
		m.Who = slices.DeleteFunc(m.Who, func(w string) bool { return !slices.Contains(r.Who, w) })
		m.SecondHuman = m.SecondHuman || r.SecondHuman
		if n, have := deciderScopeRank(r.MaxScope), deciderScopeRank(m.MaxScope); n < have || (n == have && m.MaxScope == "") {
			m.MaxScope = r.MaxScope
		}
		if kind == DeciderKindElevation && r.MaxElevation != "" &&
			(m.MaxElevation == "" || r.MaxElevationDuration() < m.MaxElevationDuration()) {
			m.MaxElevation = r.MaxElevation
		}
		if kind == DeciderKindElevation {
			m.EligibleRequesters = slices.DeleteFunc(m.EligibleRequesters, func(g string) bool { return !slices.Contains(r.EligibleRequesters, g) })
			if r.RenewAfterDuration() > m.RenewAfterDuration() {
				m.RenewAfter = r.RenewAfter
			}
		}
	}
	return m, true
}

// DeciderRuleWithin is the first field in which r reaches further than bound for kind: "who" (an entry
// bound does not name), "second_human" (bound sets it, r does not), "max_scope", "max_elevation",
// "eligible_requesters" (a group bound does not name) or "renew_after" (shorter than bound's); "" when r
// is within bound. A ticket_pattern is never wider: every pattern on the chain binds at the request.
func DeciderRuleWithin(kind string, r, bound DeciderRule) string {
	switch {
	case slices.ContainsFunc(r.Who, func(w string) bool { return !slices.Contains(bound.Who, w) }):
		return "who"
	case bound.SecondHuman && !r.SecondHuman:
		return "second_human"
	case deciderScopeRank(r.MaxScope) > deciderScopeRank(bound.MaxScope):
		return "max_scope"
	case kind == DeciderKindElevation && bound.MaxElevation != "" && r.MaxElevationDuration() > bound.MaxElevationDuration():
		return "max_elevation"
	case kind == DeciderKindElevation && slices.ContainsFunc(r.EligibleRequesters, func(g string) bool { return !slices.Contains(bound.EligibleRequesters, g) }):
		return "eligible_requesters"
	case kind == DeciderKindElevation && r.RenewAfterDuration() < bound.RenewAfterDuration():
		return "renew_after"
	}
	return ""
}

// deciderScopeRank orders a rule's max_scope by how far it reaches: once, until, then run. An empty
// max_scope reaches as far as run.
func deciderScopeRank(s ApprovalScope) int {
	switch s {
	case ScopeOnce:
		return 1
	case ScopeUntil:
		return 2
	}
	return 3
}
