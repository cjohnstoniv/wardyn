// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"testing"
)

func TestMeetDeciderRules(t *testing.T) {
	egress := func(who ...string) DeciderRule {
		if who == nil {
			who = []string{}
		}
		return DeciderRule{Kinds: []string{DeciderKindEgressDomain, DeciderKindMCPCall}, Who: who}
	}
	elevation := func(d string) DeciderRule {
		return DeciderRule{Kinds: []string{DeciderKindElevation}, Who: []string{"unit_admins"}, MaxElevation: d}
	}
	withScope := func(r DeciderRule, s ApprovalScope) DeciderRule { r.MaxScope = s; return r }
	withSecond := func(r DeciderRule) DeciderRule { r.SecondHuman = true; return r }
	for _, tc := range []struct {
		name  string
		kind  string
		chain []DeciderRule
		want  DeciderRule
	}{
		{"who intersection keeps the nearest order", DeciderKindEgressDomain,
			[]DeciderRule{egress("group:b", "unit_admins", "group:a"), egress("group:a", "group:b", "owner")},
			DeciderRule{Kinds: []string{DeciderKindEgressDomain}, Who: []string{"group:b", "group:a"}}},
		{"a disjoint chain names nobody", DeciderKindEgressDomain,
			[]DeciderRule{egress("group:x"), egress("unit_admins")},
			DeciderRule{Kinds: []string{DeciderKindEgressDomain}, Who: []string{}}},
		{"second_human set by any rule holds", DeciderKindEgressDomain,
			[]DeciderRule{egress("unit_admins"), withSecond(egress("unit_admins"))},
			DeciderRule{Kinds: []string{DeciderKindEgressDomain}, Who: []string{"unit_admins"}, SecondHuman: true}},
		{"an empty max_scope and once give once", DeciderKindEgressDomain,
			[]DeciderRule{egress("owner"), withScope(egress("owner"), ScopeOnce)},
			DeciderRule{Kinds: []string{DeciderKindEgressDomain}, Who: []string{"owner"}, MaxScope: ScopeOnce}},
		{"until and run give until", DeciderKindEgressDomain,
			[]DeciderRule{withScope(egress("owner"), ScopeUntil), withScope(egress("owner"), ScopeRun)},
			DeciderRule{Kinds: []string{DeciderKindEgressDomain}, Who: []string{"owner"}, MaxScope: ScopeUntil}},
		{"run and an empty max_scope give the named run", DeciderKindEgressDomain,
			[]DeciderRule{egress("owner"), withScope(egress("owner"), ScopeRun)},
			DeciderRule{Kinds: []string{DeciderKindEgressDomain}, Who: []string{"owner"}, MaxScope: ScopeRun}},
		{"8h and 4h give 4h", DeciderKindElevation,
			[]DeciderRule{elevation("8h"), elevation("4h")},
			DeciderRule{Kinds: []string{DeciderKindElevation}, Who: []string{"unit_admins"}, MaxElevation: "4h"}},
		{"a single rule is that rule, for the one kind", DeciderKindMCPCall,
			[]DeciderRule{withScope(egress("owner", "group:a"), ScopeOnce)},
			DeciderRule{Kinds: []string{DeciderKindMCPCall}, Who: []string{"owner", "group:a"}, MaxScope: ScopeOnce}},
	} {
		got, ok := MeetDeciderRules(tc.kind, tc.chain)
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: MeetDeciderRules = %+v, %v; want %+v, true", tc.name, got, ok, tc.want)
		}
	}
	if got, ok := MeetDeciderRules(DeciderKindEgressDomain, nil); ok {
		t.Errorf("an empty chain = %+v, true; want no rule", got)
	}
}

func TestDeciderRuleWithin(t *testing.T) {
	base := DeciderRule{Kinds: []string{DeciderKindEgressDomain}, Who: []string{"unit_admins", "group:a"}, SecondHuman: true, MaxScope: ScopeUntil}
	elevation := DeciderRule{Kinds: []string{DeciderKindElevation}, Who: []string{"unit_admins"}, MaxElevation: "4h"}
	with := func(r DeciderRule, f func(*DeciderRule)) DeciderRule {
		r.Who = append([]string{}, r.Who...)
		f(&r)
		return r
	}
	for _, tc := range []struct {
		name     string
		kind     string
		r, bound DeciderRule
		want     string
	}{
		{"an equal rule", DeciderKindEgressDomain, base, base, ""},
		{"who: an entry the bound does not name", DeciderKindEgressDomain, with(base, func(r *DeciderRule) { r.Who = append(r.Who, "group:b") }), base, "who"},
		{"who: fewer entries", DeciderKindEgressDomain, with(base, func(r *DeciderRule) { r.Who = []string{"group:a"} }), base, ""},
		{"second_human: left off", DeciderKindEgressDomain, with(base, func(r *DeciderRule) { r.SecondHuman = false }), base, "second_human"},
		{"second_human: added", DeciderKindEgressDomain, base, with(base, func(r *DeciderRule) { r.SecondHuman = false }), ""},
		{"max_scope: run over until", DeciderKindEgressDomain, with(base, func(r *DeciderRule) { r.MaxScope = ScopeRun }), base, "max_scope"},
		{"max_scope: empty over until", DeciderKindEgressDomain, with(base, func(r *DeciderRule) { r.MaxScope = "" }), base, "max_scope"},
		{"max_scope: once under until", DeciderKindEgressDomain, with(base, func(r *DeciderRule) { r.MaxScope = ScopeOnce }), base, ""},
		{"max_elevation: 8h over 4h", DeciderKindElevation, with(elevation, func(r *DeciderRule) { r.MaxElevation = "8h" }), elevation, "max_elevation"},
		{"max_elevation: 1h under 4h", DeciderKindElevation, with(elevation, func(r *DeciderRule) { r.MaxElevation = "1h" }), elevation, ""},
		{"max_elevation: an equal rule", DeciderKindElevation, elevation, elevation, ""},
	} {
		if got := DeciderRuleWithin(tc.kind, tc.r, tc.bound); got != tc.want {
			t.Errorf("%s: DeciderRuleWithin = %q, want %q", tc.name, got, tc.want)
		}
	}
}
