// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBoundaryMeetNarrowsAndKeepsUnnamedKnobs: the boundary narrows the knobs it names, re-intersects
// eligible grants, read an open policy as their own allowlist, and leave every other knob as it was.
func TestBoundaryMeetNarrowsAndKeepsUnnamedKnobs(t *testing.T) {
	inBoundary := types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.anthropic.com","header":"x-api-key","secret_name":"anthropic"}`)}
	outside := types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"outside.example","header":"x-api-key","secret_name":"outside"}`)}
	allowed := []string{"api.anthropic.com", "pypi.org"}
	grants := []types.GrantSpec{inBoundary}
	cc := types.CC3
	boundary := types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{AllowedDomains: &allowed, MinConfinementClass: &cc, EligibleGrants: &grants}}

	open := types.RunPolicySpec{AllowAllEgress: true, MinConfinementClass: types.CC1,
		AllowedMethods: []string{"GET"}, EligibleGrants: []types.GrantSpec{inBoundary, outside}}
	got, _, changes, err := boundaryMeet(boundary, open, types.GovernanceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.AllowAllEgress || !slices.Equal(got.AllowedDomains, allowed) {
		t.Errorf("open policy met as allow_all=%v %v, want the boundary's allowlist", got.AllowAllEgress, got.AllowedDomains)
	}
	if got.MinConfinementClass != types.CC3 {
		t.Errorf("floor = %s, want CC3", got.MinConfinementClass)
	}
	if len(got.EligibleGrants) != 1 || got.EligibleGrants[0].Kind != inBoundary.Kind || string(got.EligibleGrants[0].Scope) != string(inBoundary.Scope) {
		t.Errorf("grants = %+v, want only the one the boundary dominate", got.EligibleGrants)
	}
	if !slices.Equal(got.AllowedMethods, []string{"GET"}) {
		t.Errorf("allowed_methods = %v, want the policy's own (the boundary do not name it)", got.AllowedMethods)
	}
	if len(changes) == 0 {
		t.Error("no changes reported")
	}

	// A policy already inside the boundary is unchanged and reports nothing.
	inside := types.RunPolicySpec{MinConfinementClass: types.CC3, AllowedDomains: []string{"pypi.org"}}
	got, _, changes, err = boundaryMeet(boundary, inside, types.GovernanceLimits{})
	if err != nil || len(changes) != 0 || !slices.Equal(got.AllowedDomains, []string{"pypi.org"}) {
		t.Errorf("inside policy: %+v changes %+v err %v", got, changes, err)
	}
}

// TestBoundaryMeetCC1FloorIsNoOp: a floor at CC1, the fresh-install seed's, bounds nothing, so a
// policy with no floor of its own meets it unchanged and nothing is reported narrowed.
func TestBoundaryMeetCC1FloorIsNoOp(t *testing.T) {
	cc := types.CC1
	seeded := types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{MinConfinementClass: &cc}}
	got, _, changes, err := boundaryMeet(seeded, types.RunPolicySpec{}, types.GovernanceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.MinConfinementClass != "" || len(changes) != 0 {
		t.Errorf("min_confinement_class = %q, changes %+v: want the seed's CC1 floor left out of the meet", got.MinConfinementClass, changes)
	}
}
