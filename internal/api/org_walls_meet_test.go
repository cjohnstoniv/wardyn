// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestWallsMeetNarrowsAndKeepsUnnamedKnobs: the walls narrow the knobs they name, re-intersect
// eligible grants, read an open policy as their own allowlist, and leave every other knob as it was.
func TestWallsMeetNarrowsAndKeepsUnnamedKnobs(t *testing.T) {
	inWalls := types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.anthropic.com","header":"x-api-key","secret_name":"anthropic"}`)}
	outside := types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"outside.example","header":"x-api-key","secret_name":"outside"}`)}
	allowed := []string{"api.anthropic.com", "pypi.org"}
	grants := []types.GrantSpec{inWalls}
	cc := types.CC3
	walls := types.OrgWalls{Enabled: true, Spec: types.OrgWallsSpec{AllowedDomains: &allowed, MinConfinementClass: &cc, EligibleGrants: &grants}}

	open := types.RunPolicySpec{AllowAllEgress: true, MinConfinementClass: types.CC1,
		AllowedMethods: []string{"GET"}, EligibleGrants: []types.GrantSpec{inWalls, outside}}
	got, _, changes, err := wallsMeet(walls, open, types.GovernanceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.AllowAllEgress || !slices.Equal(got.AllowedDomains, allowed) {
		t.Errorf("open policy met as allow_all=%v %v, want the walls' allowlist", got.AllowAllEgress, got.AllowedDomains)
	}
	if got.MinConfinementClass != types.CC3 {
		t.Errorf("floor = %s, want CC3", got.MinConfinementClass)
	}
	if len(got.EligibleGrants) != 1 || got.EligibleGrants[0].Kind != inWalls.Kind || string(got.EligibleGrants[0].Scope) != string(inWalls.Scope) {
		t.Errorf("grants = %+v, want only the one the walls dominate", got.EligibleGrants)
	}
	if !slices.Equal(got.AllowedMethods, []string{"GET"}) {
		t.Errorf("allowed_methods = %v, want the policy's own (the walls do not name it)", got.AllowedMethods)
	}
	if len(changes) == 0 {
		t.Error("no changes reported")
	}

	// A policy already inside the walls is unchanged and reports nothing.
	inside := types.RunPolicySpec{MinConfinementClass: types.CC3, AllowedDomains: []string{"pypi.org"}}
	got, _, changes, err = wallsMeet(walls, inside, types.GovernanceLimits{})
	if err != nil || len(changes) != 0 || !slices.Equal(got.AllowedDomains, []string{"pypi.org"}) {
		t.Errorf("inside policy: %+v changes %+v err %v", got, changes, err)
	}
}
