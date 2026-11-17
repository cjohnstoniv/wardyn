// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An api_key grant marked requires_approval is refused when a policy is
// written, as env_secret and file_secret are: every door that injects a key
// drops an approval-gated grant, so no run could ever use it. The same grant
// without the flag is accepted, on every validator a write goes through.
func TestAPIKeyRequiresApprovalRefusedAtSave(t *testing.T) {
	for _, gated := range []bool{false, true} {
		g := types.GrantSpec{Kind: types.GrantAPIKey, RequiresApproval: gated, Scope: apiKeyScope(t, "api.example.test", "corp-key")}
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{g}}
		errs := map[string]error{
			"validateEligibleGrantMode strict":  validateEligibleGrantMode(0, g, true),
			"validateEligibleGrantMode lenient": validateEligibleGrantMode(0, g, false),
			"validatePolicySpec":                validatePolicySpec(spec),
			"validateAuthoredPolicySpec":        validateAuthoredPolicySpec(spec),
			"validatePolicySpecLenient":         validatePolicySpecLenient(spec),
		}
		for name, err := range errs {
			if gated {
				if err == nil || !strings.Contains(err.Error(), "eligible_grants[0]: api_key cannot require approval") {
					t.Errorf("requires_approval=true, %s: err = %v, want the api_key approval refusal", name, err)
				}
			} else if err != nil {
				t.Errorf("requires_approval=false, %s: err = %v, want accepted", name, err)
			}
		}
	}
}

// A ceiling an older release stored may still hold an approval-gated api_key
// grant. The clamp forces approval on a member's api_key grant under it, and
// the validation every clamp caller runs afterwards refuses the result, so no
// run is created with a key nothing could inject.
func TestAPIKeyClampedToApprovalIsRefusedByValidation(t *testing.T) {
	scope := apiKeyScope(t, "api.example.test", "corp-key")
	ceiling := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: scope, RequiresApproval: true}}}
	proposed := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: scope}}}

	clamped, _ := composer.Clamp(proposed, ceiling, types.GovernanceLimits{})
	if len(clamped.EligibleGrants) != 1 || !clamped.EligibleGrants[0].RequiresApproval {
		t.Fatalf("clamp = %+v, want the one api_key grant forced to require approval", clamped.EligibleGrants)
	}
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: clamped.EligibleGrants}
	if err := validatePolicySpec(spec); err == nil || !strings.Contains(err.Error(), "api_key cannot require approval") {
		t.Errorf("validatePolicySpec(clamped) = %v, want the api_key approval refusal", err)
	}
}
