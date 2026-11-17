// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateAuthoredSpecBase is validateAuthoredPolicySpec without the api_key approval rule, for a
// door that judges that rule later against the stored row it replaces.
func validateAuthoredSpecBase(spec types.RunPolicySpec) error {
	if err := validatePolicySpec(spec); err != nil {
		return err
	}
	for i, g := range spec.EligibleGrants {
		if err := authoredGrantRefusal(fmt.Sprintf("eligible_grants[%d]", i), g); err != nil {
			return err
		}
	}
	return nil
}

// hasGatedAPIKey reports whether grants holds an api_key grant that requires approval.
func hasGatedAPIKey(grants []types.GrantSpec) bool {
	return slices.ContainsFunc(grants, func(g types.GrantSpec) bool { return g.Kind == types.GrantAPIKey && g.RequiresApproval })
}

// authoredAPIKeyApprovalRefusal refuses an api_key grant marked requires_approval
// where a person or a file authors it now, as env_secret and file_secret are
// refused: every door that wires an api_key into a run drops an approval-gated
// grant (persistRunGrants, probeInjections, mintRecordAPIKeyInjections) and its
// mint answers without the key, so no run could ever use it.
//
// It is NOT part of validatePolicySpec: a policy, ceiling or default saved
// before this rule still loads, clamps, overlays and launches as it always did,
// its key left out of every run, and no launch refuses one. Only a save does.
// held is what an earlier write already carried; a grant equal to one of them is
// not added by this save and is left alone.
func authoredAPIKeyApprovalRefusal(grants, held []types.GrantSpec) error {
	for i, g := range grants {
		if g.Kind != types.GrantAPIKey || !g.RequiresApproval || slices.ContainsFunc(held, func(h types.GrantSpec) bool { return sameGrant(h, g) }) {
			continue
		}
		return fmt.Errorf("eligible_grants[%d]: api_key cannot require approval — every door that injects a key drops an "+
			"approval-gated grant, so no run could ever use it", i)
	}
	return nil
}

// sameGrant reports whether two grants are equal, their scopes compared in
// JSONB's canonical form (keys sorted) and not as the bytes a store or a
// re-marshal left them in.
func sameGrant(a, b types.GrantSpec) bool {
	canon := func(raw json.RawMessage) json.RawMessage {
		if c, err := types.CanonicalJSONB(raw); err == nil {
			return c
		}
		return raw
	}
	a.Scope, b.Scope = canon(a.Scope), canon(b.Scope)
	return reflect.DeepEqual(a, b)
}

// rawEligibleGrants is the eligible_grants a stored or authored policy object
// carries, read leniently: nil when there are none or the object does not decode.
func rawEligibleGrants(raw json.RawMessage) []types.GrantSpec {
	var spec struct {
		EligibleGrants []types.GrantSpec `json:"eligible_grants"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &spec) != nil {
		return nil
	}
	return spec.EligibleGrants
}
