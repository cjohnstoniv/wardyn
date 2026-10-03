// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
)

// TestInjectionRuleFromScopeCarriesRequireTLS (a residual): this decoder is
// what BINDS the policy key. The rule it returns rides runner.InjectionGrant
// into the proxy's own config, and the proxy's plain lane refuses a cleartext
// request for a rule that sets it — so a decoder that silently dropped the field
// would leave an operator's declaration inert with every gate green.
func TestInjectionRuleFromScopeCarriesRequireTLS(t *testing.T) {
	for _, c := range []struct {
		name  string
		scope string
		want  bool
	}{
		{"set", `{"host":"h.test","secret_name":"k","require_tls":true}`, true},
		{"explicit false", `{"host":"h.test","secret_name":"k","require_tls":false}`, false},
		{"absent is today's behaviour", `{"host":"h.test","secret_name":"k"}`, false},
	} {
		rule, err := injectionRuleFromScope([]byte(c.scope))
		if err != nil {
			t.Fatalf("%s: injectionRuleFromScope: %v", c.name, err)
		}
		if rule.RequireTLS != c.want {
			t.Errorf("%s: RequireTLS = %v, want %v", c.name, rule.RequireTLS, c.want)
		}
	}
}

// TestInjectionRuleFromScopeRefusesAMisspelledKey: the strict decode is what
// keeps require_tls from failing OPEN on a typo.
//
// A plain Unmarshal ignores an unknown key, so an operator who wrote
// "requiretls" got a policy that was ACCEPTED, a rule with RequireTLS false, and
// a credential on the cleartext transport they had just tried to forbid — with
// nothing anywhere saying so. The refusal lands at the write boundary, where the
// text is still in front of them.
func TestInjectionRuleFromScopeRefusesAMisspelledKey(t *testing.T) {
	for _, scope := range []string{
		`{"host":"h.test","secret_name":"k","requiretls":true}`,
		`{"host":"h.test","secret_name":"k","require-tls":true}`,
		`{"host":"h.test","secret_name":"k","requires_tls":true}`,
	} {
		if _, err := injectionRuleFromScope([]byte(scope)); err == nil {
			t.Errorf("scope %s decoded without error — a misspelled require_tls must be refused, "+
				"never silently read as false", scope)
		}
	}
	// The shape Wardyn itself authors stays valid, or every api_key grant breaks.
	if _, err := injectionRuleFromScope([]byte(
		`{"host":"h.test","header":"Authorization","format":"Bearer %s","secret_name":"k"}`)); err != nil {
		t.Fatalf("the four-key scope Wardyn authors must still decode: %v", err)
	}
}

// TestInjectionRuleFromScopeRoundTripsThePinRoutes: the method-and-path set the Azure route gate's rule
// carries survives the strict decode into the rule the sidecar is configured from, admits exactly its
// pairs, and a malformed pair is refused at the write boundary rather than shipped as a rule that
// withholds the credential from everything.
func TestInjectionRuleFromScopeRoundTripsThePinRoutes(t *testing.T) {
	rule, err := injectionRuleFromScope([]byte(`{"host":"h.test","secret_name":"k","require_tls":true,
		"pin_routes":[{"method":"POST","path":"/anthropic/v1/messages"},{"method":"POST","path":"/anthropic/v1/messages/count_tokens"}]}`))
	if err != nil {
		t.Fatalf("injectionRuleFromScope: %v", err)
	}
	if !rule.Pinned() || len(rule.PinRoutes) != 2 {
		t.Fatalf("rule = %+v, want a two-route pin", rule)
	}
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/anthropic/v1/messages", true},
		{"POST", "/anthropic/v1/messages/count_tokens", true},
		{"GET", "/anthropic/v1/messages", false},
		{"POST", "/anthropic/v1/messages/", false},
		{"POST", "/anthropic/v1/messages/batches", false},
		{"POST", "/openai/files", false},
	} {
		if got := rule.AllowsInjection(c.method, c.path, "beta=true"); got != c.want {
			t.Errorf("AllowsInjection(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
	for _, bad := range []string{
		`{"method":"","path":"/x"}`, `{"method":"post","path":"/x"}`, `{"method":"POST","path":"x"}`, `{"method":"POST","path":""}`,
	} {
		if _, err := injectionRuleFromScope([]byte(`{"host":"h.test","secret_name":"k","pin_routes":[` + bad + `]}`)); err == nil {
			t.Errorf("pin_routes entry %s decoded without error", bad)
		}
	}
	if _, err := injectionRuleFromScope([]byte(`{"host":"h.test","secret_name":"k","pin_routes":[{"method":"POST","path":"/x","extra":1}]}`)); err == nil {
		t.Error("a pin_routes entry with an unknown key decoded without error")
	}
}
