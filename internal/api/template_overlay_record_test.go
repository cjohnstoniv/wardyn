// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRecordedTemplatePolicyDoesNotKeepTheSourcePoliciesGrants: `wardyn record save --template`
// writes the keys a recording decided (cmd/wardyn recordedPolicyKeys), the narrowing ones even when
// empty. Laid onto a source policy that holds a grant and a wide allowlist, they leave no grant and
// only the recorded hosts; a recording that left eligible_grants out would keep the grant.
func TestRecordedTemplatePolicyDoesNotKeepTheSourcePoliciesGrants(t *testing.T) {
	source := types.RunPolicySpec{
		AllowedDomains: []string{"wide.example.com"}, AllowAllEgress: true, AutoStopAfterSec: 0,
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.example.com","header":"x-api-key"}`)}},
		DeniedDomains:  []string{"evil.example.com"},
	}
	recorded := json.RawMessage(`{"allow_all_egress":false,"allowed_domains":["pypi.org"],"eligible_grants":[],"first_use_approval":"deny_with_review","min_confinement_class":"CC2"}`)
	got, err := templatePolicyOverlay(source, recorded)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.EligibleGrants) != 0 || got.AllowAllEgress || !slices.Equal(got.AllowedDomains, []string{"pypi.org"}) {
		t.Errorf("recorded policy over the source = %+v, want no grant, no allow-all and only pypi.org", got)
	}
	// What a recording never decides still inherits.
	if !slices.Equal(got.DeniedDomains, []string{"evil.example.com"}) {
		t.Errorf("denied_domains = %v, want the source's kept", got.DeniedDomains)
	}

	// The failure this prevents: the same recording without the empty list keeps the source's grant.
	missing := json.RawMessage(`{"allow_all_egress":false,"allowed_domains":["pypi.org"]}`)
	widened, err := templatePolicyOverlay(source, missing)
	if err != nil || len(widened.EligibleGrants) != 1 {
		t.Errorf("an overlay without eligible_grants gave %+v, %v; the test no longer shows why the key must be written", widened.EligibleGrants, err)
	}
}

// TestRecordedTemplateKeepsOrInheritsAllowedMethods: the proposal is post-clamp, so a ceiling's methods
// can be in it. Written to the template they stay GET over an unrestricted source; a proposal with none
// leaves the key out, so a GET-only source keeps GET rather than widening to every method.
func TestRecordedTemplateKeepsOrInheritsAllowedMethods(t *testing.T) {
	open := types.RunPolicySpec{AllowedDomains: []string{"wide.example.com"}}
	withMethods := json.RawMessage(`{"allowed_domains":["pypi.org"],"allowed_methods":["GET"],"eligible_grants":[]}`)
	got, err := templatePolicyOverlay(open, withMethods)
	if err != nil || !slices.Equal(got.AllowedMethods, []string{"GET"}) {
		t.Errorf("a recorded [GET] over an unrestricted source gave %v, %v; want GET kept", got.AllowedMethods, err)
	}
	getOnly := types.RunPolicySpec{AllowedDomains: []string{"wide.example.com"}, AllowedMethods: []string{"GET"}}
	noMethods := json.RawMessage(`{"allowed_domains":["pypi.org"],"eligible_grants":[]}`)
	kept, err := templatePolicyOverlay(getOnly, noMethods)
	if err != nil || !slices.Equal(kept.AllowedMethods, []string{"GET"}) {
		t.Errorf("a proposal with no methods over a GET-only source gave %v, %v; want GET inherited", kept.AllowedMethods, err)
	}
}
