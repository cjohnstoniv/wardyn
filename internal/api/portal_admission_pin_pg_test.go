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
// person's authority moves with it: the pin still bounds every launch, the user type still comes from the
// portal row, and the person still enters a unit through a membership naming that user type, because the
// stored groups are empty. The run lands only in the pinned unit, and a user_type capability grant applies.
func TestPortalAdmission_PinUserTypeAndMembershipHoldWithoutGroup_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	ctx := context.Background()
	const contractor = "contractor"
	// The type is seeded first: CreateUserType refuses an id a subject row already names.
	if _, err := e.st.CreateUserType(ctx, types.UserType{ID: contractor, Name: contractor, Priority: 1, CreatedBy: "seed"}); err != nil {
		t.Fatalf("seed the user type: %v", err)
	}
	// The type is a member of the team (higher priority, so it is the default pick) and of an unrelated unit.
	for _, m := range []struct {
		unit     types.GovernanceUnit
		priority int
	}{{e.team, 2}, {e.other, 1}} {
		if _, err := e.st.UpsertGovernanceMembership(ctx, types.GovernanceMembership{ID: uuid.New(),
			SubjectType: types.CapabilitySubjectUserType, Subject: contractor, UnitID: m.unit.ID, Priority: m.priority}); err != nil {
			t.Fatalf("seed the %s membership: %v", m.unit.Name, err)
		}
	}
	// The agent is restricted: only an allow naming it lets anyone launch it.
	if err := e.st.SetCapabilityRestriction(ctx, capAgent, "claude-code", true, "seed"); err != nil {
		t.Fatalf("restrict the agent: %v", err)
	}

	d := e.portal(t, e.bind(t), &e.plat.ID, func(d *types.Delegate) { d.UserType = contractor })
	if d.Group != "" || d.RequiredClaim != nil || !d.PortalManagedAdmission {
		t.Fatalf("fixture portal group %q claim %v managed %v, want no group, no claim match, portal managed", d.Group, d.RequiredClaim, d.PortalManagedAdmission)
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

	// The grant keyed on the portal row's user type now applies, and the default pick is the member unit
	// under the pin, reached with no group and no claim.
	w := e.post(t, "/api/v1/runs", tok, pinRun(""))
	if w.Code != http.StatusCreated {
		t.Fatalf("launch after the user_type grant = %d %s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	run, err := e.st.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatalf("read the created run: %v", err)
	}
	if run.UnitID == nil || *run.UnitID != e.team.ID {
		t.Errorf("the run landed in unit %v, want the pinned platform's team %s", run.UnitID, e.team.ID)
	}

	// The person is a member of the unrelated unit too, and the pin still refuses it.
	if w := e.post(t, "/api/v1/runs", tok, pinRun(e.other.Name)); !portalScopeRefused(w) {
		t.Errorf("launch in the unit outside the pin = %d %s, want 403 portal_unit_scope", w.Code, w.Body.String())
	}
	if rows := e.rows("run.create"); len(rows) != 1 {
		t.Errorf("run.create rows = %d, want 1 (the refused launches leave no run)", len(rows))
	}
}
