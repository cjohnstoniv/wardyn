// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

import "testing"

// TestCountsAsActivity_Matrix pins the one rule every ingest path shares: only an allowed decision is the
// run doing something. A refusal, a hold, a model call's token counts and the toolgate's poll of the request
// it is blocked on are not. A brokered allow (a credential mint, a recording) is a real allowed request.
func TestCountsAsActivity_Matrix(t *testing.T) {
	cases := []struct {
		name string
		d    DecisionLog
		want bool
	}{
		{"policy allow", DecisionLog{Decision: Allow, RuleSource: "policy:allowlist"}, true},
		{"allow with no rule source", DecisionLog{Decision: Allow}, true},
		{"site-config internal host allow", DecisionLog{Decision: Allow, RuleSource: "site-config:internal-host"}, true},
		{"brokered mint allow", DecisionLog{Decision: Allow, RuleSource: "brokered:mint"}, true},
		{"brokered recording allow", DecisionLog{Decision: Allow, RuleSource: "brokered:recording"}, true},
		{"brokered scan-result allow", DecisionLog{Decision: Allow, RuleSource: "brokered:scan-result"}, true},
		{"brokered sso-token allow", DecisionLog{Decision: Allow, RuleSource: "brokered:sso-token"}, true},
		{"approvals poll allow", DecisionLog{Decision: Allow, RuleSource: "brokered:approvals"}, false},
		{"approvals poll deny", DecisionLog{Decision: Deny, RuleSource: "brokered:approvals"}, false},
		{"policy deny", DecisionLog{Decision: Deny, RuleSource: "policy:denied"}, false},
		{"builtin deny", DecisionLog{Decision: Deny, RuleSource: "builtin:private-ip"}, false},
		{"dial failed deny", DecisionLog{Decision: Deny, RuleSource: "builtin:dial-failed"}, false},
		{"reauth timeout deny", DecisionLog{Decision: Deny, RuleSource: "credential:reauth-timeout"}, false},
		{"first-use hold", DecisionLog{Decision: Pending, RuleSource: "policy:first-use"}, false},
		{"model usage row", DecisionLog{Decision: Allow, RuleSource: "llm:usage", Usage: &ModelUsage{}}, false},
		{"empty decision", DecisionLog{RuleSource: "policy:allowlist"}, false},
	}
	for _, c := range cases {
		if got := CountsAsActivity(c.d); got != c.want {
			t.Errorf("%s: CountsAsActivity = %v, want %v", c.name, got, c.want)
		}
	}
}
