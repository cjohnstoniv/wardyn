// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// apiKeyRuleGrant is an api_key eligible/inline grant naming every field of the
// InjectionRule the proxy will write onto the forwarded request.
func apiKeyRuleGrant(host, secret, header, format string) types.GrantSpec {
	return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{
		"host": host, "secret_name": secret, "header": header, "format": format,
	})}
}

// TestFilterMemberGrants_ReHeaderedGrantDropped is the F097 regression on the
// CLAMP half. The member pairing check compared (host, secret, known_hosts)
// only, so a member or profile grant that kept the operator's blessed pairing
// and named a DIFFERENT header — or a different format — matched the operator's
// ceiling entry and was kept. That is a re-homing of the operator's blessed
// secret: the proxy writes the authored header verbatim onto the forwarded
// request (internal/egress/proxy/inject.go; the brokered LLM lane strips the
// four known credential headers and then sets the authored one) and relays the
// upstream response to the sandbox verbatim, so an upstream that echoes the
// offending header hands the operator's key to the sandbox.
func TestFilterMemberGrants_ReHeaderedGrantDropped(t *testing.T) {
	h := newHarness(t)
	blessed := apiKeyRuleGrant("api.corp.example", "corp-key", "x-api-key", "%s")
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{EligibleGrants: []types.GrantSpec{blessed}}

	for name, g := range map[string]types.GrantSpec{
		"different header": apiKeyRuleGrant("api.corp.example", "corp-key", "anthropic-version", "%s"),
		"different format": apiKeyRuleGrant("api.corp.example", "corp-key", "x-api-key", "Bearer %s"),
	} {
		kept, warns, code, err := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{g})
		if code != 0 || err != nil {
			t.Fatalf("%s: code=%d err=%v, want (0,nil) — dropped, not errored", name, code, err)
		}
		if len(kept) != 0 {
			t.Errorf("%s: kept=%d, want 0 — the operator never authorized this header/format for that secret", name, len(kept))
		}
		if len(warns) != 1 {
			t.Errorf("%s: warns=%d, want 1", name, len(warns))
		}
	}

	// The operator's OWN authored rule still passes, header/format and all —
	// and so does the same rule written with the fields left empty, which
	// injectionRuleFromScope resolves to the same Authorization/"Bearer %s".
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{blessed}); len(kept) != 1 {
		t.Errorf("the operator's own authored rule: kept=%d, want 1", len(kept))
	}
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		apiKeyRuleGrant("api.corp.example", "corp-key", "", ""),
	}}
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil,
		[]types.GrantSpec{apiKeyRuleGrant("api.corp.example", "corp-key", "Authorization", "Bearer %s")}); len(kept) != 1 {
		t.Errorf("defaulted header/format must equal the explicit Authorization/\"Bearer %%s\": kept=0, want 1")
	}
}

// TestValidateEligibleGrant_FormatRule is the F097 regression on the VALIDATION
// half. The sink does fmt.Sprintf(format, secret) unconditionally
// (formatInjectionValue), and the INTEGRATION authoring path has always rejected
// a format that is not exactly one %s with no CR/LF — while the policy path for
// the identical wire field checked nothing at all. One rule
// (validInjectionFormat), both authoring paths.
func TestValidateEligibleGrant_FormatRule(t *testing.T) {
	bad := map[string]string{
		"two verbs":  "tok=%s;copy=%s",
		"other verb": "%d %s",
		"no verb":    "Bearer ",
		"line break": "Bearer %s\r\nX-Evil: 1",
	}
	for name, format := range bad {
		g := apiKeyRuleGrant("api.corp.example", "corp-key", "Authorization", format)
		if err := validateEligibleGrant(0, g); err == nil {
			t.Errorf("%s: validateEligibleGrant(format=%q) = nil, want an error", name, format)
		}
	}
	for name, format := range map[string]string{"bearer": "Bearer %s", "raw": "%s", "empty": ""} {
		g := apiKeyRuleGrant("api.corp.example", "corp-key", "Authorization", format)
		if err := validateEligibleGrant(0, g); err != nil {
			t.Errorf("%s: validateEligibleGrant(format=%q) = %v, want nil", name, format, err)
		}
	}
}
