// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The scoped prune pins (governance_prune_under.go): a prune write naming ?under= is refused 409 when its
// unit has been moved out of that subtree, on each of the four prune routes, for a security admin and a
// unit admin, applied directly or held; a held one goes stale when its unit is moved out before it is
// approved; and the SDK's own prune, with a move between its last read and its delete, deletes nothing.
// A unit admin naming a unit above their own gets the same 409, and a held write keeps the payload a
// replica before under decodes. They run on the unit-admin tree of governance_unit_admin_pg_test.go.

// pruneTarget is a unit p below ua-a1 and c below p, with a membership, a grant and deciders rules on c
// and an empty unit c2 beside c, all seeded directly.
type pruneTarget struct {
	p, c, c2 types.GovernanceUnit
	m        types.GovernanceMembership
	g        types.GovernanceUnitAdmin
}

func (e *unitAdminEnv) pruneTarget() pruneTarget {
	e.t.Helper()
	p := e.child(e.a1)
	c := e.child(p)
	t := pruneTarget{p: p, c: c, c2: e.child(p)}
	t.m = treeMember(e.t, e.govEnv, c, types.CapabilitySubjectUser, e.fresh("sub-pu-member"), 1, false)
	t.g = e.grant(c, types.CapabilitySubjectUser, e.fresh("sub-pu-admin"))
	if w := e.admin(http.MethodPut, decidersPath(c), decidersBody(egressOwnerRule())); w.Code != http.StatusOK {
		e.t.Fatalf("seed the deciders of %s = %d %s", c.Name, w.Code, w.Body)
	}
	return t
}

// writes is the prune's four writes, as the SDK sends them under under: c's grant revoked, c's rules
// cleared, c's membership deleted, and the empty unit c2 deleted.
func (t pruneTarget) writes(under types.GovernanceUnit) [][3]string {
	q := "?under=" + under.ID.String()
	return [][3]string{
		{http.MethodDelete, "/api/v1/governance/units/" + t.c.ID.String() + "/admins/" + t.g.ID.String() + q, ""},
		{http.MethodPut, decidersPath(t.c) + q, `{"rules":[]}`},
		{http.MethodDelete, "/api/v1/governance/memberships/" + t.m.ID.String() + q, ""},
		{http.MethodDelete, "/api/v1/governance/units/" + t.c2.ID.String() + q, ""},
	}
}

// moveTo re-parents u below parent with the admin token, as another admin's write would.
func (e *unitAdminEnv) moveTo(u, parent types.GovernanceUnit) {
	e.t.Helper()
	if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+u.ID.String(), e.restate(u, map[string]any{"parent_id": parent.ID})); w.Code != http.StatusOK {
		e.t.Fatalf("move %s below %s = %d %s", u.Name, parent.Name, w.Code, w.Body)
	}
}

// intact fails the test unless t's grant, rules, membership and c2 are all still stored.
func (e *unitAdminEnv) intact(t pruneTarget, what string) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.pg.GetUnitAdmin(ctx, t.c.ID, t.g.ID); err != nil {
		e.t.Errorf("%s: the grant is gone: %v", what, err)
	}
	if len(e.storedDeciders(t.c)) != 1 {
		e.t.Errorf("%s: the rules are %v, want the one seeded", what, e.storedDeciders(t.c))
	}
	if _, err := store.GetGovernanceMembershipQ(ctx, e.pool, t.m.ID, false); err != nil {
		e.t.Errorf("%s: the membership is gone: %v", what, err)
	}
	if _, err := e.pg.GetGovernanceUnit(ctx, t.c2.ID); err != nil {
		e.t.Errorf("%s: %s is gone: %v", what, t.c2.Name, err)
	}
}

// TestPruneUnderRefusesAUnitMovedOut: after p moves from ua-a1 to ua-a2, each prune write naming ua-a1 is
// refused 409 and writes and holds nothing, with the switch off and on, for a security admin and for an
// admin of ua-a, who governs both. Naming ua-a2, where p now is, each applies; a malformed under is a 400.
func TestPruneUnderRefusesAUnitMovedOut(t *testing.T) {
	for _, on := range []bool{false, true} {
		e := newUnitAdminEnv(t, on)
		for _, caller := range []struct {
			who string
			c   *http.Cookie
		}{{"a security admin", e.alice}, {"an admin of ua-a", e.adminA}} {
			target := e.pruneTarget()
			e.moveTo(target.p, e.a2)
			for _, wr := range target.writes(e.a1) {
				if w := e.call(caller.c, wr[0], wr[1], wr[2]); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceUnitNotUnder {
					t.Errorf("switch %v, %s: %s %s = %d %s, want 409 %s", on, caller.who, wr[0], wr[1], w.Code, w.Body, reasonGovernanceUnitNotUnder)
				}
			}
			e.intact(target, caller.who)
			if n := e.pendingCount(); n != 0 {
				t.Errorf("switch %v, %s: %d changes held, want none", on, caller.who, n)
			}
		}
	}
	e := newUnitAdminEnv(t, false)
	target := e.pruneTarget()
	e.moveTo(target.p, e.a2)
	for _, wr := range target.writes(e.a2) {
		if w := e.call(e.alice, wr[0], wr[1], wr[2]); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
			t.Errorf("%s %s naming the unit it is below = %d %s, want it applied", wr[0], wr[1], w.Code, w.Body)
		}
	}
	if _, err := e.pg.GetGovernanceUnit(context.Background(), target.c2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("%s after its delete: %v, want it gone", target.c2.Name, err)
	}
	if w := e.call(e.alice, http.MethodDelete, "/api/v1/governance/units/"+target.c.ID.String()+"?under=nope", ""); w.Code != http.StatusBadRequest ||
		wireReason(t, w) != reasonInvalidIDParam {
		t.Errorf("a malformed under = %d %s, want 400 %s", w.Code, w.Body, reasonInvalidIDParam)
	}
}

// TestPruneUnderHeldGoesStale: with the switch on, the four prune writes naming ua-a1 are held while p is
// below it; p then moves to ua-a2, and approving each answers stale and applies nothing.
func TestPruneUnderHeldGoesStale(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	target := e.pruneTarget()
	var held []types.GovernanceChange
	for _, wr := range target.writes(e.a1) {
		held = append(held, e.pending(e.call(e.alice, wr[0], wr[1], wr[2])))
	}
	e.moveTo(target.p, e.a2)
	for _, ch := range held {
		if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
			t.Errorf("approving the held %s %s after the move = %d %s, want 409 %s", ch.TargetKind, ch.Op, w.Code, w.Body, reasonGovernanceChangeStale)
		}
	}
	e.intact(target, "after the approvals")
}

// TestPruneUnderAboveAUnitAdminsScope: an admin of ua-a1 naming ua-a, a unit above the one they govern
// that the target is below, gets the same 409 as for ua-b, which the target was never below, and nothing
// is written or held: the answer tells them nothing about the tree above their units. Naming ua-a1 applies.
func TestPruneUnderAboveAUnitAdminsScope(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	target := e.pruneTarget()
	for _, under := range []types.GovernanceUnit{e.a, e.b} {
		for _, wr := range target.writes(under) {
			if w := e.call(e.adminA1, wr[0], wr[1], wr[2]); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceUnitNotUnder {
				t.Errorf("an admin of ua-a1 naming %s: %s %s = %d %s, want 409 %s", under.Name, wr[0], wr[1], w.Code, w.Body, reasonGovernanceUnitNotUnder)
			}
		}
	}
	e.intact(target, "an admin of ua-a1")
	if n := e.pendingCount(); n != 0 {
		t.Errorf("%d changes held, want none", n)
	}
	unit := target.writes(e.a1)[3]
	if w := e.call(e.adminA1, unit[0], unit[1], unit[2]); w.Code != http.StatusNoContent {
		t.Errorf("an admin of ua-a1 naming ua-a1: %s %s = %d %s, want 204", unit[0], unit[1], w.Code, w.Body)
	}
}

// The held deciders write and revoke as a replica before under decodes them (decodeHeldPayload is strict):
// a field added to either payload leaves a change that replica cannot decide, pending, holding its target.
type (
	olderUnitDecidersPayload struct {
		UnitID uuid.UUID           `json:"unit_id"`
		Rules  []types.DeciderRule `json:"rules"`
	}
	olderUnitAdminPayload struct {
		ID          uuid.UUID                   `json:"id,omitzero"`
		UnitID      uuid.UUID                   `json:"unit_id"`
		SubjectType types.CapabilitySubjectType `json:"subject_type"`
		Subject     string                      `json:"subject"`
	}
)

// TestPruneUnderHeldAppliesAndKeepsItsPayload: with the switch on, a revoke and a deciders clear naming
// ua-a1 are held. Each stored payload decodes as the older shape, and its base is not the write's own
// state alone, so a replica before under answers stale and applies nothing instead of leaving it pending.
// With the unit still below ua-a1, each approval applies.
func TestPruneUnderHeldAppliesAndKeepsItsPayload(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	target := e.pruneTarget()
	ctx := context.Background()
	plainRevoke, _, err := unitAdminState(ctx, e.pool, unitAdminPayload{UnitID: target.c.ID, SubjectType: target.g.SubjectType, Subject: target.g.Subject}, false)
	if err != nil {
		t.Fatal(err)
	}
	plainClear, _, err := unitDecidersState(ctx, e.pool, target.c.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct {
		what  string
		older any
		plain any
	}{{"revoke", &olderUnitAdminPayload{}, plainRevoke}, {"deciders clear", &olderUnitDecidersPayload{}, plainClear}} {
		wr := target.writes(e.a1)[i]
		held, err := e.pg.GetGovernanceChange(ctx, e.pending(e.call(e.alice, wr[0], wr[1], wr[2])).ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := decodeHeldPayload(held.Payload, c.older); err != nil {
			t.Errorf("the held %s payload %s does not decode on a replica before under: %v", c.what, held.Payload, err)
		}
		if held.BaseHash == computeETag(c.plain) {
			t.Errorf("the held %s has the base of a write without under: a replica before under would apply it after a move", c.what)
		}
		if w := e.call(e.bob, http.MethodPost, approvePath(held.ID), ""); w.Code != http.StatusOK {
			t.Errorf("approving the held %s with the unit still below %s = %d %s, want 200", c.what, e.a1.Name, w.Code, w.Body)
		}
	}
	if _, err := e.pg.GetUnitAdmin(ctx, target.c.ID, target.g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the grant after its approved revoke: %v, want it gone", err)
	}
	if rules := e.storedDeciders(target.c); len(rules) != 0 {
		t.Errorf("the rules after their approved clear are %v, want none", rules)
	}
}

// TestPruneUnderSDKMoveBeforeDelete: the SDK's own `set --under ua-a1 --prune` of a document without c2,
// where another admin moves c2 out between the SDK's last read and its delete, deletes nothing: the delete
// is refused and c2 stays where it was moved.
func TestPruneUnderSDKMoveBeforeDelete(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	c2 := e.child(e.a1)
	ts := httptest.NewServer(panicFails(t, e.srv.Handler()))
	t.Cleanup(ts.Close)
	moved := false
	move := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/governance/units/"+c2.ID.String() && !moved {
			moved = true
			e.moveTo(c2, e.a2)
		}
		return ts.Client().Transport.RoundTrip(r)
	})
	c := &client.Client{BaseURL: ts.URL, Token: adminToken, HTTPClient: &http.Client{Transport: move}}
	ctx := context.Background()
	doc, err := c.GetGovernanceUnder(ctx, e.a1.Name)
	if err != nil {
		t.Fatal(err)
	}
	var units []client.GovernanceUnit
	for _, u := range doc.Units {
		if u.ID != c2.ID {
			units = append(units, u)
		}
	}
	doc.Units = units
	_, err = c.ApplyGovernanceResultUnder(ctx, doc, e.a1.Name, true)
	var apiErr *client.APIError
	if !moved || !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Reason != reasonGovernanceUnitNotUnder {
		t.Fatalf("apply with a move before the delete (moved %v) = %v, want 409 %s", moved, err, reasonGovernanceUnitNotUnder)
	}
	if got := e.profile(c2.ID); got.ParentID == nil || *got.ParentID != e.a2.ID {
		t.Errorf("%s is below %v, want it kept below %s", c2.Name, got.ParentID, e.a2.Name)
	}
}
