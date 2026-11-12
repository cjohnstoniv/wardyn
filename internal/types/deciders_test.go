// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDeciderKindsAreTheClosedSet(t *testing.T) {
	want := []string{"egress_domain", "ado_escalation", "mcp_call", "kube_call", "credential", "push_content", "elevation", "image", "file_download"}
	if !slices.Equal(DeciderKinds, want) {
		t.Fatalf("DeciderKinds = %v, want %v", DeciderKinds, want)
	}
	if slices.Contains(DeciderKinds, string(ApprovalToolCall)) {
		t.Errorf("tool_call is split into ado_escalation and mcp_call and must not be a decider kind")
	}
}

func TestValidateDeciderRulesAccepts(t *testing.T) {
	for name, rules := range map[string][]DeciderRule{
		"none":                           nil,
		"egress owner and group":         {{Kinds: []string{"egress_domain"}, Who: []string{"owner", "group:eng-leads"}, MaxScope: ScopeRun}},
		"ado and mcp peers second human": {{Kinds: []string{"ado_escalation", "mcp_call"}, Who: []string{"unit_admins", "group:eng-peers"}, SecondHuman: true}},
		"credential second human once":   {{Kinds: []string{"credential"}, Who: []string{"unit_admins"}, SecondHuman: true, MaxScope: ScopeOnce}},
		"push content":                   {{Kinds: []string{"push_content"}, Who: []string{"unit_admins"}, SecondHuman: true, MaxScope: ScopeOnce}},
		"empty who means security only":  {{Kinds: []string{"egress_domain"}, Who: nil}},
		"elevation":                      {{Kinds: []string{"elevation"}, Who: []string{"unit_admins", "group:support-leads"}, MaxElevation: "4h", EligibleRequesters: []string{"group:support-leads"}}},
		"elevation bounds":               {{Kinds: []string{"elevation"}, MaxElevation: "15m", EligibleRequesters: []string{"group:support-leads"}}, {Kinds: []string{"egress_domain"}, Who: []string{"owner"}, MaxScope: ScopeUntil}},
		"kube_call like mcp_call":        {{Kinds: []string{"kube_call", "mcp_call"}, Who: []string{"owner", "group:sre"}, MaxScope: ScopeOnce}},
		"kube_call second human":         {{Kinds: []string{"kube_call"}, Who: []string{"unit_admins"}, SecondHuman: true}},
		"image":                          {{Kinds: []string{"image"}, Who: []string{"unit_admins", "group:platform"}}},
		"file_download":                  {{Kinds: []string{"file_download"}, Who: []string{"group:dlp"}, SecondHuman: true}},
		"file_download forced":           {{Kinds: []string{"file_download"}, Who: []string{"unit_admins"}}},
		"every kind once": {
			{Kinds: []string{"egress_domain", "ado_escalation", "mcp_call"}, Who: []string{"group:a"}},
			{Kinds: []string{"credential", "push_content"}, Who: []string{"unit_admins"}, SecondHuman: true},
			{Kinds: []string{"elevation"}, MaxElevation: "24h", EligibleRequesters: []string{"group:support-leads"}},
			{Kinds: []string{"kube_call"}, Who: []string{"group:b"}},
			{Kinds: []string{"image"}, Who: []string{"unit_admins"}},
			{Kinds: []string{"file_download"}, Who: []string{"unit_admins"}},
		},
	} {
		got, err := ValidateDeciderRules(rules)
		if err != nil {
			t.Errorf("%s: %v, want accepted", name, err)
			continue
		}
		for i, r := range got {
			if r.Kinds == nil || r.Who == nil {
				t.Errorf("%s: rule %d has a nil kinds or who after validation, want empty lists that encode as []", name, i)
			}
		}
	}
}

func TestValidateDeciderRulesNormalizes(t *testing.T) {
	got, err := ValidateDeciderRules([]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"group:  Eng-Leads "}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"group:eng-leads"}; !slices.Equal(got[0].Who, want) {
		t.Errorf("who = %v, want %v (the group folded as a grant's subject is)", got[0].Who, want)
	}
}

func TestValidateDeciderRulesRefuses(t *testing.T) {
	egress := DeciderRule{Kinds: []string{"egress_domain"}, Who: []string{"unit_admins"}}
	for name, tc := range map[string]struct {
		rules []DeciderRule
		index int
		want  string
	}{
		"no kinds":                  {[]DeciderRule{{Who: []string{"owner"}}}, 0, "kinds"},
		"unknown kind":              {[]DeciderRule{egress, {Kinds: []string{"capability"}}}, 1, "capability"},
		"tool_call is split":        {[]DeciderRule{{Kinds: []string{"tool_call"}}}, 0, "ado_escalation"},
		"credential_reauth":         {[]DeciderRule{{Kinds: []string{"credential_reauth"}}}, 0, "credential_reauth"},
		"kind twice in a rule":      {[]DeciderRule{{Kinds: []string{"mcp_call", "mcp_call"}}}, 0, "mcp_call"},
		"kind twice across rules":   {[]DeciderRule{egress, {Kinds: []string{"egress_domain"}, Who: []string{"owner"}}}, 1, "egress_domain"},
		"security listed":           {[]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"security"}}}, 0, "security"},
		"manager is cut":            {[]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"manager"}}}, 0, "manager"},
		"delegate is cut":           {[]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"delegate"}}}, 0, "delegate"},
		"unknown who":               {[]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"everyone"}}}, 0, "everyone"},
		"empty group":               {[]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"group:  "}}}, 0, "group"},
		"non-ascii group":           {[]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"group:opsK"}}}, 0, "group"},
		"who twice":                 {[]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"group:A", "group:a"}}}, 0, "group:a"},
		"nine who":                  {[]DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"group:1", "group:2", "group:3", "group:4", "group:5", "group:6", "group:7", "group:8", "group:9"}}}, 0, "8"},
		"owner on credential":       {[]DeciderRule{{Kinds: []string{"credential"}, Who: []string{"owner"}, SecondHuman: true}}, 0, "owner"},
		"owner on push_content":     {[]DeciderRule{{Kinds: []string{"push_content"}, Who: []string{"owner"}, SecondHuman: true}}, 0, "owner"},
		"owner on elevation":        {[]DeciderRule{{Kinds: []string{"elevation"}, Who: []string{"owner"}, MaxElevation: "1h"}}, 0, "owner"},
		"owner with second human":   {[]DeciderRule{{Kinds: []string{"mcp_call"}, Who: []string{"owner"}, SecondHuman: true}}, 0, "owner"},
		"credential no second":      {[]DeciderRule{{Kinds: []string{"credential"}, Who: []string{"unit_admins"}}}, 0, "second_human"},
		"push_content no second":    {[]DeciderRule{{Kinds: []string{"push_content"}, Who: []string{"unit_admins"}}}, 0, "second_human"},
		"second human on elevation": {[]DeciderRule{{Kinds: []string{"elevation"}, SecondHuman: true, MaxElevation: "1h"}}, 0, "second_human"},
		"always scope":              {[]DeciderRule{{Kinds: []string{"egress_domain"}, MaxScope: ScopeAlways}}, 0, "always"},
		"unknown scope":             {[]DeciderRule{{Kinds: []string{"egress_domain"}, MaxScope: "forever"}}, 0, "forever"},
		"scope on elevation":        {[]DeciderRule{{Kinds: []string{"elevation"}, MaxScope: ScopeOnce, MaxElevation: "1h"}}, 0, "max_scope"},
		"elevation without cap":     {[]DeciderRule{{Kinds: []string{"elevation"}}}, 0, "max_elevation"},
		"cap not a duration":        {[]DeciderRule{{Kinds: []string{"elevation"}, MaxElevation: "soon"}}, 0, "max_elevation"},
		"cap under 15m":             {[]DeciderRule{{Kinds: []string{"elevation"}, MaxElevation: "14m"}}, 0, "max_elevation"},
		"cap over 24h":              {[]DeciderRule{{Kinds: []string{"elevation"}, MaxElevation: "25h"}}, 0, "max_elevation"},
		"cap on another kind":       {[]DeciderRule{{Kinds: []string{"egress_domain"}, MaxElevation: "1h"}}, 0, "max_elevation"},
		"owner on image":            {[]DeciderRule{{Kinds: []string{"image"}, Who: []string{"owner"}}}, 0, "owner"},
		"owner on file_download":    {[]DeciderRule{{Kinds: []string{"file_download"}, Who: []string{"owner"}}}, 0, "owner"},
		"second human on image":     {[]DeciderRule{{Kinds: []string{"image"}, SecondHuman: true}}, 0, "second_human"},
		"scope on image":            {[]DeciderRule{{Kinds: []string{"image"}, MaxScope: ScopeOnce}}, 0, "max_scope"},
		"scope on file_download":    {[]DeciderRule{{Kinds: []string{"file_download"}, MaxScope: ScopeOnce}}, 0, "max_scope"},
		"cap on image":              {[]DeciderRule{{Kinds: []string{"image"}, MaxElevation: "1h"}}, 0, "max_elevation"},
		"image mixed":               {[]DeciderRule{{Kinds: []string{"image", "mcp_call"}}}, 0, "image"},
		"file_download mixed":       {[]DeciderRule{{Kinds: []string{"file_download", "credential"}, SecondHuman: true}}, 0, "file_download"},
		"elevation mixed":           {[]DeciderRule{{Kinds: []string{"elevation", "egress_domain"}, MaxElevation: "1h"}}, 0, "elevation"},
	} {
		_, err := ValidateDeciderRules(tc.rules)
		var re *DeciderRuleError
		if !errors.As(err, &re) {
			t.Errorf("%s: err = %v, want a *DeciderRuleError", name, err)
			continue
		}
		if re.Index != tc.index {
			t.Errorf("%s: names rule %d, want %d (%v)", name, re.Index, tc.index, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %q does not mention %q", name, err, tc.want)
		}
		if !strings.Contains(err.Error(), "rules["+string(rune('0'+tc.index))+"]") {
			t.Errorf("%s: %q does not name the rule index", name, err)
		}
	}
}

func TestDeciderRuleFor(t *testing.T) {
	rules := []DeciderRule{{Kinds: []string{"egress_domain", "mcp_call"}, Who: []string{"owner"}}, {Kinds: []string{"credential"}, Who: []string{"unit_admins"}, SecondHuman: true}}
	if r, ok := DeciderRuleFor(rules, "mcp_call"); !ok || !slices.Equal(r.Who, []string{"owner"}) {
		t.Errorf("mcp_call = %+v %v", r, ok)
	}
	if _, ok := DeciderRuleFor(rules, "ado_escalation"); ok {
		t.Errorf("a rule for mcp_call must not bind ado_escalation")
	}
}

func TestDeciderGroupOf(t *testing.T) {
	if g, ok := DeciderGroupOf("group:eng"); !ok || g != "eng" {
		t.Errorf("group:eng = %q %v", g, ok)
	}
	for _, w := range []string{"owner", "unit_admins", "group:", "eng"} {
		if _, ok := DeciderGroupOf(w); ok {
			t.Errorf("%q is not a group decider", w)
		}
	}
}

func TestValidateDeciderRulesForcesFileDownloadSecondHuman(t *testing.T) {
	got, err := ValidateDeciderRules([]DeciderRule{{Kinds: []string{"file_download"}, Who: []string{"unit_admins"}}})
	if err != nil || !got[0].SecondHuman {
		t.Fatalf("file_download = %+v, %v, want second_human stored as true", got, err)
	}
}

// "240m" and "4h" are one cap: the stored and hashed form is one canonical duration string.
func TestValidateDeciderRulesCanonicalMaxElevation(t *testing.T) {
	for in, want := range map[string]string{"240m": "4h", "4h": "4h", "4h0m0s": "4h", "15m": "15m", "900s": "15m", "90m": "1h30m", "24h": "24h", "1440m": "24h"} {
		got, err := ValidateDeciderRules([]DeciderRule{{Kinds: []string{"elevation"}, MaxElevation: in, EligibleRequesters: []string{"group:support-leads"}}})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got[0].MaxElevation != want {
			t.Errorf("%q stored as %q, want %q", in, got[0].MaxElevation, want)
		}
		if d := got[0].MaxElevationDuration(); d != mustParse(t, in) {
			t.Errorf("%q: duration %v changed by normalizing", in, d)
		}
	}
}

func mustParse(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
