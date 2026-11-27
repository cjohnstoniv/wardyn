// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types_test

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func allowRule(host string) types.HostRule {
	return types.HostRule{Host: host, Effect: types.HostRuleAllow}
}

// TestRequestRulesHosts: a rule sits on one of the definition's own hosts named exactly, with no port or
// port 443. A wildcard entry, another port, or a host the definition does not name is refused at write,
// as is a rule the host-rule grammar refuses.
func TestRequestRulesHosts(t *testing.T) {
	for name, d := range map[string]types.ComponentDefinition{
		"no rules":                  {Hosts: []string{"api.example.com"}},
		"an empty list":             {Hosts: []string{"api.example.com"}, RequestRules: []types.HostRule{}},
		"a bare host":               {Hosts: []string{"api.example.com"}, RequestRules: []types.HostRule{allowRule("api.example.com")}},
		"a host on port 443":        {Hosts: []string{"api.example.com:443"}, RequestRules: []types.HostRule{allowRule("api.example.com")}},
		"exact beside a wildcard":   {Hosts: []string{"*.example.com", "api.example.com"}, RequestRules: []types.HostRule{allowRule("api.example.com")}},
		"exact beside another port": {Hosts: []string{"api.example.com:8443", "api.example.com"}, RequestRules: []types.HostRule{allowRule("api.example.com")}},
		"every effect, methods, paths": {Hosts: []string{"api.example.com"}, RequestRules: []types.HostRule{
			{Host: "api.example.com", Methods: []string{"GET"}, Paths: []string{"/v1"}, Effect: types.HostRuleAllow},
			{Host: "api.example.com", Methods: []string{"POST"}, Paths: []string{"/v1/charges"}, Effect: types.HostRuleHold},
			{Host: "api.example.com", Effect: types.HostRuleDeny},
		}},
		"sixty-four rules": {Hosts: []string{"api.example.com"}, RequestRules: func() []types.HostRule {
			out := make([]types.HostRule, types.MaxHostRulesPerSet)
			for i := range out {
				out[i] = types.HostRule{Host: "api.example.com", Paths: []string{fmt.Sprintf("/p%d", i)}, Effect: types.HostRuleAllow}
			}
			return out
		}()},
	} {
		if err := d.Validate(proxy.ValidDomainEntry, true); err != nil {
			t.Errorf("%s: Validate = %v, want nil", name, err)
		}
	}
	// An organisation's row may name an address; a rule on it follows the same host rule.
	org := types.ComponentDefinition{Hosts: []string{"10.0.0.5:443"}, RequestRules: []types.HostRule{allowRule("10.0.0.5")}}
	if err := org.Validate(proxy.ValidDomainEntry, false); err != nil {
		t.Errorf("an organisation row's address host: Validate = %v, want nil", err)
	}

	for name, tc := range map[string]struct {
		d      types.ComponentDefinition
		wantIn string
	}{
		"a host named only by a wildcard": {types.ComponentDefinition{Hosts: []string{"*.example.com"},
			RequestRules: []types.HostRule{allowRule("api.example.com")}}, "request_rules[0].host"},
		"a host named only on another port": {types.ComponentDefinition{Hosts: []string{"api.example.com:8443"},
			RequestRules: []types.HostRule{allowRule("api.example.com")}}, "request_rules[0].host"},
		"a host the definition does not name": {types.ComponentDefinition{Hosts: []string{"api.example.com"},
			RequestRules: []types.HostRule{allowRule("api.example.com"), allowRule("other.example.com")}}, "request_rules[1].host"},
		"no hosts at all": {types.ComponentDefinition{RequestRules: []types.HostRule{allowRule("api.example.com")}}, "request_rules[0].host"},
		"a wildcard rule host": {types.ComponentDefinition{Hosts: []string{"*.example.com"},
			RequestRules: []types.HostRule{allowRule("*.example.com")}}, "request_rules[0].host"},
		"a rule host with a port": {types.ComponentDefinition{Hosts: []string{"api.example.com:443"},
			RequestRules: []types.HostRule{allowRule("api.example.com:443")}}, "request_rules[0].host"},
		"an upper-case rule host": {types.ComponentDefinition{Hosts: []string{"api.example.com"},
			RequestRules: []types.HostRule{allowRule("API.example.com")}}, "request_rules[0].host"},
		"a lower-case method": {types.ComponentDefinition{Hosts: []string{"api.example.com"},
			RequestRules: []types.HostRule{{Host: "api.example.com", Methods: []string{"get"}, Effect: types.HostRuleAllow}}}, "request_rules[0].methods[0]"},
		"a path not in normal form": {types.ComponentDefinition{Hosts: []string{"api.example.com"},
			RequestRules: []types.HostRule{{Host: "api.example.com", Paths: []string{"/a/../b"}, Effect: types.HostRuleAllow}}}, "request_rules[0].paths[0]"},
		"an unknown effect": {types.ComponentDefinition{Hosts: []string{"api.example.com"},
			RequestRules: []types.HostRule{{Host: "api.example.com", Effect: "approve"}}}, "request_rules[0].effect"},
		"sixty-five rules": {types.ComponentDefinition{Hosts: []string{"api.example.com"},
			RequestRules: func() []types.HostRule {
				out := make([]types.HostRule, types.MaxHostRulesPerSet+1)
				for i := range out {
					out[i] = allowRule("api.example.com")
				}
				return out
			}()}, "request_rules: at most 64 rules"},
	} {
		err := tc.d.Validate(proxy.ValidDomainEntry, true)
		if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s: Validate = %v, want an error naming %q", name, err, tc.wantIn)
		}
	}
}

// TestComponentRuleSetOnlyNarrows: a component's rules are one set appended beside the others
// (types.AppendHostRuleSets), so for any request they never turn a deny into a hold or an allow, nor a
// hold into an allow. A property test over random sets and requests, with a fixed seed.
func TestComponentRuleSetOnlyNarrows(t *testing.T) {
	rng := rand.New(rand.NewPCG(1971, 3))
	hosts := []string{"a.example", "b.example", "c.example"}
	methods := []string{"GET", "POST", "DELETE"}
	rulePaths := []string{"/", "/a", "/a/b", "/b", "/admin"}
	reqPaths := []string{"/", "/a", "/a/b", "/a/bc", "/b/c", "/admin", "/Admin", "/%zz", "/a/../b"}
	effects := []types.HostRuleEffect{types.HostRuleAllow, types.HostRuleDeny, types.HostRuleHold}
	pick := func(from []string) string { return from[rng.IntN(len(from))] }
	some := func(from []string) []string {
		var out []string
		for _, s := range from {
			if rng.IntN(3) == 0 {
				out = append(out, s)
			}
		}
		return out
	}
	randomSet := func() types.HostRuleSet {
		s := types.HostRuleSet{Rules: make([]types.HostRule, 1+rng.IntN(4))}
		for i := range s.Rules {
			s.Rules[i] = types.HostRule{Host: pick(hosts), Methods: some(methods), Paths: some(rulePaths), Effect: effects[rng.IntN(len(effects))]}
		}
		return s
	}
	rank := map[types.HostRuleEffect]int{types.HostRuleDeny: 0, types.HostRuleHold: 1, types.HostRuleAllow: 2}
	for i := 0; i < 20000; i++ {
		base := make([]types.HostRuleSet, rng.IntN(4))
		for j := range base {
			base[j] = randomSet()
		}
		comp := randomSet()
		if err := types.ValidateHostRuleSets([]types.HostRuleSet{comp}); err != nil {
			t.Fatalf("the generator wrote an invalid set %+v: %v", comp, err)
		}
		with := types.AppendHostRuleSets(base, []types.HostRuleSet{comp})
		host, method, path := pick(hosts), pick(methods), pick(reqPaths)
		before := types.EvalHostRules(base, host, method, path)
		after := types.EvalHostRules(with, host, method, path)
		if rank[after.Effect] > rank[before.Effect] {
			t.Fatalf("%s %s %s: %s without the component's set, %s with it\nbase %+v\ncomponent %+v", method, host, path, before.Effect, after.Effect, base, comp)
		}
	}
}
