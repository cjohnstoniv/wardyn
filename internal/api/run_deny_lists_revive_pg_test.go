// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestReviveRefusesADeniedComponentOrProvider: a run created before its owner's profile (or a
// restriction on the owner) listed a component or a model provider is refused at its next revive, 403
// component_denied or model_provider_denied with a run.revive denied row, as a placement the profile
// now denies is. With the lists empty the same revive passes, and a list naming something else passes.
func TestReviveRefusesADeniedComponentOrProvider(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	const owner = "sub-rv-deny"
	const providerUID = "44444444-4444-4444-8444-444444444444"
	const otherID = "55555555-5555-4555-8555-555555555555"

	site, err := e.pg.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	site.ModelProviders = providerBlock(mpKeyProvider("deny-anthropic", providerUID, types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"}))
	if _, err := e.pg.PutSiteConfig(ctx, site); err != nil {
		t.Fatal(err)
	}
	compID := uuid.New()
	cs, ok := any(e.pg).(store.ComponentStore)
	if !ok {
		t.Fatal("the Postgres store has no component seam")
	}
	if _, err := cs.CreateComponent(ctx, types.Component{ID: compID, Name: "Prod Deploy",
		Definition: types.ComponentDefinition{Hosts: []string{"github.com"}}}); err != nil {
		t.Fatal(err)
	}
	unit := e.seedProfile("rv-deny", "pypi.org", "github.com")
	treeMember(t, e, unit, types.CapabilitySubjectUser, owner, 0, false)
	run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), UnitID: &unit.ID, CreatedBy: owner, UserType: types.UserTypeStandard,
		Agent: "claude-code", ModelProviderID: "deny-anthropic", ConfinementClass: types.CC3, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.PutRunComponents(ctx, run.ID, []types.RunComponent{{RunID: run.ID, ComponentID: &compID, Name: "Prod Deploy",
		Definition: types.ComponentDefinition{Hosts: []string{"github.com"}}, Version: 1}}); err != nil {
		t.Fatal(err)
	}
	asOwner := treeCtx(owner, nil, "")
	revive := func() *reviveError {
		_, rerr := e.srv.reviveCeilingAdmitting(asOwner, run, types.ActorHuman, owner)
		return rerr
	}
	setLimits := func(l types.GovernanceLimits) {
		t.Helper()
		unit.Limits = l
		var err error
		if unit, err = e.pg.UpsertGovernanceUnit(ctx, unit); err != nil {
			t.Fatal(err)
		}
	}
	want := func(name, reason string) {
		t.Helper()
		if rerr := revive(); rerr == nil || rerr.status != http.StatusForbidden || rerr.reason != reason {
			t.Errorf("%s: revive = %+v, want 403 %s", name, rerr, reason)
		}
	}

	if rerr := revive(); rerr != nil {
		t.Fatalf("revive under empty lists: %+v", rerr)
	}
	setLimits(types.GovernanceLimits{DenyComponents: []string{otherID}, DenyModelProviders: []string{otherID}})
	if rerr := revive(); rerr != nil {
		t.Errorf("revive under lists that name something else: %+v", rerr)
	}
	setLimits(types.GovernanceLimits{DenyComponents: []string{compID.String()}})
	want("profile denies the component", "component_denied")
	setLimits(types.GovernanceLimits{DenyModelProviders: []string{providerUID}})
	want("profile denies the provider", "model_provider_denied")
	setLimits(types.GovernanceLimits{})
	if rerr := revive(); rerr != nil {
		t.Fatalf("revive after the lists were emptied: %+v", rerr)
	}

	r := seedRestriction(t, e, "rv-deny-comp", types.RestrictionSpec{}, types.RestrictionLimits{DenyComponents: strs(compID.String())}, userMember(owner))
	want("a restriction denies the component", "component_denied")
	if _, err := e.pg.DeleteRestriction(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	seedRestriction(t, e, "rv-deny-prov", types.RestrictionSpec{}, types.RestrictionLimits{DenyModelProviders: strs(providerUID)}, userMember(owner))
	want("a restriction denies the provider", "model_provider_denied")
}
