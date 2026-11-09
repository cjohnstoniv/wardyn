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
		{"relayed allow", DecisionLog{Request: Request{Relayed: true}, Decision: Allow, RuleSource: "policy:allowlist"}, false},
		{"relayed deny", DecisionLog{Request: Request{Relayed: true}, Decision: Deny, RuleSource: "policy:denied"}, false},
		{"agent-listener allow", DecisionLog{Request: Request{Relayed: false}, Decision: Allow, RuleSource: "policy:allowlist"}, true},
		{"model usage row", DecisionLog{Decision: Allow, RuleSource: "llm:usage", Usage: &ModelUsage{}}, false},
		{"empty decision", DecisionLog{RuleSource: "policy:allowlist"}, false},
	}
	for _, c := range cases {
		if got := CountsAsActivity(c.d); got != c.want {
			t.Errorf("%s: CountsAsActivity = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCountsAsActivityFor_IgnoredHosts: an allowed request to a host on the ignore list is not activity, by
// the same exact-or-wildcard match the policy's domain lists use. A refusal never counts, listed or not, and
// an empty list changes nothing.
func TestCountsAsActivityFor_IgnoredHosts(t *testing.T) {
	allow := func(host string) DecisionLog {
		return DecisionLog{Request: Request{Host: host}, Decision: Allow, RuleSource: "policy:allowlist"}
	}
	ignore := []string{"mask.icloud.com", "*.apple.com"}
	for _, c := range []struct {
		name   string
		d      DecisionLog
		ignore []string
		want   bool
	}{
		{"listed host", allow("mask.icloud.com"), ignore, false},
		{"listed host, any case and a trailing dot", allow("MASK.iCloud.com."), ignore, false},
		{"wildcard match", allow("gdmf.apple.com"), ignore, false},
		{"the wildcard's apex is not covered", allow("apple.com"), ignore, true},
		{"a host that only shares a suffix", allow("notmask.icloud.com"), ignore, true},
		{"unlisted host", allow("github.com"), ignore, true},
		{"empty list", allow("mask.icloud.com"), nil, true},
		{"refusal of a listed host", DecisionLog{Request: Request{Host: "mask.icloud.com"}, Decision: Deny, RuleSource: "policy:denied"}, ignore, false},
		{"approvals poll", DecisionLog{Request: Request{Host: "github.com"}, Decision: Allow, RuleSource: "brokered:approvals"}, ignore, false},
	} {
		if got := CountsAsActivityFor(c.d, c.ignore); got != c.want {
			t.Errorf("%s: CountsAsActivityFor = %v, want %v", c.name, got, c.want)
		}
	}
}
