// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The seal's half of composer's host-rules order tests: a layer's sets are appended to the run's own,
// never merged, whichever side holds which set.
func TestSealHostRulesOrderIndependent(t *testing.T) {
	set := func(effect types.HostRuleEffect, method, path string) []types.HostRuleSet {
		return []types.HostRuleSet{{Rules: []types.HostRule{{Host: "api.example.com", Methods: []string{method}, Paths: []string{path}, Effect: effect}}}}
	}
	pairs := []struct {
		name string
		a, b []types.HostRuleSet
		deny [2]string
	}{
		{"GET /public vs POST /admin", set(types.HostRuleAllow, "GET", "/public"), set(types.HostRuleAllow, "POST", "/admin"), [2]string{"POST", "/admin"}},
		{"allow GET / vs deny GET /private", set(types.HostRuleAllow, "GET", "/"), set(types.HostRuleDeny, "GET", "/private"), [2]string{"GET", "/private"}},
	}
	for _, pr := range pairs {
		t.Run(pr.name, func(t *testing.T) {
			seal := func(own, layer []types.HostRuleSet) []types.HostRuleSet {
				spec := types.RunPolicySpec{HostRules: own}
				ov := layer
				decided := sealPolicy(&spec, []sealLayer{{rec: policyref.Provenance{Layer: policyref.LayerRestriction, Name: "r"}, egress: types.CeilingOverlay{HostRules: &ov}}})
				if len(decided) != 1 || decided[0].knob != "host_rules" {
					t.Errorf("seal decisions = %+v, want one host_rules decision", decided)
				}
				return spec.HostRules
			}
			ab, ba := seal(pr.a, pr.b), seal(pr.b, pr.a)
			for _, got := range [][]types.HostRuleSet{ab, ba} {
				if !types.HostRuleSetsWithin(got, pr.a) || !types.HostRuleSetsWithin(got, pr.b) {
					t.Fatalf("seal lost a layer's set: %+v", got)
				}
				if d := types.EvalHostRules(got, "api.example.com", pr.deny[0], pr.deny[1]); d.Effect != types.HostRuleDeny {
					t.Errorf("%s %s = %s after the seal, want deny", pr.deny[0], pr.deny[1], d.Effect)
				}
			}
			if !reflect.DeepEqual(ab, ba) {
				t.Errorf("seal depends on order:\n a,b = %+v\n b,a = %+v", ab, ba)
			}
		})
	}
}
