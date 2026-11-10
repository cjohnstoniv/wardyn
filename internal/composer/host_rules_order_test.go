// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Host rules compose by keeping each layer's set (types.HostRuleSet): first match over concatenated rules
// would let one layer's allow through the other layer's no-match deny. Each pair below runs in both orders
// through compose, clamp and leq; the seal's half is internal/api's TestSealHostRulesOrderIndependent.

const ruleHost = "api.example.com"

func ruleSet(effect types.HostRuleEffect, method, path string) []types.HostRuleSet {
	return []types.HostRuleSet{{Rules: []types.HostRule{{Host: ruleHost, Methods: []string{method}, Paths: []string{path}, Effect: effect}}}}
}

type hostRulePair struct {
	name string
	a, b []types.HostRuleSet
	// want is each request's effect under both layers, whichever comes first.
	want map[[2]string]types.HostRuleEffect
}

func hostRulePairs() []hostRulePair {
	return []hostRulePair{
		{
			name: "GET /public vs POST /admin",
			a:    ruleSet(types.HostRuleAllow, "GET", "/public"),
			b:    ruleSet(types.HostRuleAllow, "POST", "/admin"),
			want: map[[2]string]types.HostRuleEffect{
				{"GET", "/public"}: types.HostRuleDeny, {"POST", "/admin"}: types.HostRuleDeny, {"GET", "/admin"}: types.HostRuleDeny,
			},
		},
		{
			name: "allow GET / vs deny GET /private",
			a:    ruleSet(types.HostRuleAllow, "GET", "/"),
			b:    ruleSet(types.HostRuleDeny, "GET", "/private"),
			want: map[[2]string]types.HostRuleEffect{
				{"GET", "/private"}: types.HostRuleDeny, {"GET", "/private/x"}: types.HostRuleDeny, {"GET", "/public"}: types.HostRuleDeny,
			},
		},
	}
}

// checkBoth asserts that r holds both operands' sets unchanged and decides every request as want says.
func checkBoth(t *testing.T, label string, pr hostRulePair, r []types.HostRuleSet) {
	t.Helper()
	if !types.HostRuleSetsWithin(r, pr.a) || !types.HostRuleSetsWithin(r, pr.b) {
		t.Fatalf("%s: %+v lost a layer's set (want both %+v and %+v kept)", label, r, pr.a, pr.b)
	}
	for req, want := range pr.want {
		if got := types.EvalHostRules(r, ruleHost, req[0], req[1]).Effect; got != want {
			t.Errorf("%s: %s %s = %s, want %s", label, req[0], req[1], got, want)
		}
	}
}

func TestHostRulesComposeOrderIndependent(t *testing.T) {
	for _, pr := range hostRulePairs() {
		t.Run(pr.name, func(t *testing.T) {
			compose := func(base, ov []types.HostRuleSet) []types.HostRuleSet {
				out, _, err := ApplyOverlay(Authority{Ceiling: types.RunPolicySpec{HostRules: base}}, Overlay{Ceiling: types.CeilingOverlay{HostRules: &ov}})
				if err != nil {
					t.Fatalf("ApplyOverlay: %v", err)
				}
				return out.Ceiling.HostRules
			}
			ab, ba := compose(pr.a, pr.b), compose(pr.b, pr.a)
			checkBoth(t, "compose(a,b)", pr, ab)
			checkBoth(t, "compose(b,a)", pr, ba)
			if !reflect.DeepEqual(ab, ba) {
				t.Errorf("compose depends on order:\n a,b = %+v\n b,a = %+v", ab, ba)
			}
			if err := ValidateOverlay(Authority{Ceiling: types.RunPolicySpec{HostRules: pr.a}}, Overlay{Ceiling: types.CeilingOverlay{HostRules: &pr.b}}); err != nil {
				t.Errorf("ValidateOverlay refused a set that only narrows: %v", err)
			}
		})
	}
}

func TestHostRulesClampOrderIndependent(t *testing.T) {
	for _, pr := range hostRulePairs() {
		t.Run(pr.name, func(t *testing.T) {
			clamped := func(proposed, ceiling []types.HostRuleSet) []types.HostRuleSet {
				out, _ := Clamp(types.RunPolicySpec{HostRules: proposed}, types.RunPolicySpec{HostRules: ceiling}, types.GovernanceLimits{})
				return out.HostRules
			}
			ab, ba := clamped(pr.a, pr.b), clamped(pr.b, pr.a)
			checkBoth(t, "clamp(a under b)", pr, ab)
			checkBoth(t, "clamp(b under a)", pr, ba)
			if !reflect.DeepEqual(ab, ba) {
				t.Errorf("clamp depends on order:\n a,b = %+v\n b,a = %+v", ab, ba)
			}
		})
	}
}

func TestHostRulesLeqOrderIndependent(t *testing.T) {
	for _, pr := range hostRulePairs() {
		t.Run(pr.name, func(t *testing.T) {
			spec := func(sets ...[]types.HostRuleSet) types.RunPolicySpec {
				return types.RunPolicySpec{HostRules: slices.Concat(sets...)}
			}
			var none types.GovernanceLimits
			ab, ba := spec(pr.a, pr.b), spec(pr.b, pr.a)
			for _, both := range []types.RunPolicySpec{ab, ba} {
				if !Leq(both, spec(pr.a), none, none) || !Leq(both, spec(pr.b), none, none) {
					t.Errorf("both sets %+v should be within each one alone", both.HostRules)
				}
				if Leq(spec(pr.a), both, none, none) || Leq(spec(pr.b), both, none, none) {
					t.Errorf("one set alone should not be within both %+v", both.HostRules)
				}
				if !Leq(both, spec(), none, none) || Leq(spec(), both, none, none) {
					t.Errorf("no rules should be the widest: %+v", both.HostRules)
				}
			}
			if !Leq(ab, ba, none, none) || !Leq(ba, ab, none, none) {
				t.Errorf("leq reads the order of sets: %+v vs %+v", ab.HostRules, ba.HostRules)
			}
		})
	}
}
