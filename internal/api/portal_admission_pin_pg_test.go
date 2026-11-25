// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A portal with no group and no claim match admits on the token checks alone, and nothing else about the
// person's authority moves with it: the user type comes from the portal row (the exchange copies it into the
// grant in portalExchange, and the helper's grant models that copy), the person enters a unit through a
// membership naming that user type, and the pin bounds the pick. The type's top-priority membership is a unit
// outside the pin, which is never a default pick for an issuer portal's person: with no run_in the run lands in
// the pin unit itself, and naming the outside unit is refused. The named unit under the pin launches. A portal
// pinned to that other unit, on the same type, lands there by default: the control that the type's own default
// pick really is the other unit, so landing in the pin above is the pin's doing.
func TestPortalAdmission_PinUserTypeAndMembershipHoldWithoutGroup_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	ctx := context.Background()
	const contractor = "contractor"
	// The type is seeded first: CreateUserType refuses an id a subject row already names.
	if _, err := e.st.CreateUserType(ctx, types.UserType{ID: contractor, Name: contractor, Priority: 1, CreatedBy: "seed"}); err != nil {
		t.Fatalf("seed the user type: %v", err)
	}
	// The type is a member of the unit outside the pin at the higher priority (the default pick, since the
	// rows are ordered by priority DESC, store/subject_tier.go subjectPrecedence) and of the team under the pin.
	for _, m := range []struct {
		unit     types.GovernanceUnit
		priority int
	}{{e.other, 2}, {e.team, 1}} {
		if _, err := e.st.UpsertGovernanceMembership(ctx, types.GovernanceMembership{ID: uuid.New(),
			SubjectType: types.CapabilitySubjectUserType, Subject: contractor, UnitID: m.unit.ID, Priority: m.priority}); err != nil {
			t.Fatalf("seed the %s membership: %v", m.unit.Name, err)
		}
	}
	// The agent is restricted: only an allow naming it lets anyone launch it.
	if err := e.st.SetCapabilityRestriction(ctx, capAgent, "claude-code", true, "seed"); err != nil {
		t.Fatalf("restrict the agent: %v", err)
	}

	typed := func(d *types.Delegate) { d.UserType = contractor }
	d := e.portal(t, e.bind(t), &e.plat.ID, typed)
	if d.UserType != contractor || d.Group != "" || d.RequiredClaim != nil || !d.PortalManagedAdmission {
		t.Fatalf("fixture portal type %q group %q claim %v managed %v, want the contractor type, no group, no claim match, portal managed", d.UserType, d.Group, d.RequiredClaim, d.PortalManagedAdmission)
	}
	_, tok := e.person(t, d, "alice")

	// The membership alone gives the unit, not the capability: the restricted agent is refused.
	if w := e.post(t, "/api/v1/runs", tok, pinRun("")); w.Code != http.StatusForbidden || errorReason(w) != "capability_agent" {
		t.Fatalf("launch before the user_type grant = %d %s, want 403 capability_agent", w.Code, w.Body.String())
	}
	if _, err := e.st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{SubjectType: types.CapabilitySubjectUserType, Subject: contractor,
		Capability: capAgent, Value: "claude-code", Effect: types.CapabilityAllow, CreatedBy: "seed"}); err != nil {
		t.Fatalf("grant the agent to the user type: %v", err)
	}

	// The grant keyed on the portal row's user type now applies. The membership outside the pin is not picked
	// by default, so no run_in lands in the pin unit; naming the outside unit is refused, and the team under the
	// pin launches when named.
	if got := e.launchUnit(t, tok, ""); got != e.plat.ID {
		t.Errorf("launch with no run_in landed in unit %s, want the pin unit %s", got, e.plat.ID)
	}
	if w := e.post(t, "/api/v1/runs", tok, pinRun(e.other.Name)); !portalScopeRefused(w) {
		t.Errorf("launch with run_in %q (outside the pin) = %d %s, want 403 portal_unit_scope", e.other.Name, w.Code, w.Body.String())
	}
	if got := e.launchUnit(t, tok, e.team.Name); got != e.team.ID {
		t.Errorf("the pinned portal's run landed in unit %s, want the team %s under the pin", got, e.team.ID)
	}

	// Control: the same type through a portal pinned to the other unit lands there by default, so without a pin
	// on the platform the default pick is the other unit.
	free := e.portal(t, e.bind(t), &e.other.ID, typed)
	_, freeTok := e.person(t, free, "bob")
	if got := e.launchUnit(t, freeTok, ""); got != e.other.ID {
		t.Errorf("the default pick through a portal pinned to the other unit landed in %s, want %s", got, e.other.ID)
	}

	// Control: a portal whose row carries the standard type, on the same pin, gives its person none of the
	// contractor type's grant, so the restricted agent is refused (the agent check runs before any unit check).
	// The type applied is the portal row's.
	plain := e.portal(t, e.bind(t), &e.plat.ID)
	_, plainTok := e.person(t, plain, "carol")
	if w := e.post(t, "/api/v1/runs", plainTok, pinRun("")); w.Code != http.StatusForbidden || errorReason(w) != "capability_agent" {
		t.Errorf("a standard-type portal's launch = %d %s, want 403 capability_agent", w.Code, w.Body.String())
	}
	if rows := e.rows("run.create"); len(rows) != 3 {
		t.Errorf("run.create rows = %d, want 3 (the pinned portal's team run, its pin-default run and the control's other-unit run)", len(rows))
	}
}

// launchUnit launches through tok in runIn, requires a 201 and returns the unit the run landed in.
func (e *authorityEnv) launchUnit(t *testing.T, tok, runIn string) uuid.UUID {
	t.Helper()
	w := e.post(t, "/api/v1/runs", tok, pinRun(runIn))
	if w.Code != http.StatusCreated {
		t.Fatalf("launch with run_in %q = %d %s, want 201", runIn, w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	run, err := e.st.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("read the created run: %v", err)
	}
	if run.UnitID == nil {
		t.Fatalf("run %s has no unit", run.ID)
	}
	return *run.UnitID
}
