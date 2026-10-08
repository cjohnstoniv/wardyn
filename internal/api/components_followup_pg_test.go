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

func (e componentsPG) liftPath(id uuid.UUID) string {
	return "/api/v1/permissions/availability/component/" + id.String()
}

// availabilityWrites counts the capability.availability.write rows for a component id with the given bit.
func (e componentsPG) availabilityWrites(id uuid.UUID, restricted bool) int {
	n := 0
	for _, ev := range e.h.audit.snapshot() {
		var d map[string]any
		if ev.Action == "capability.availability.write" && json.Unmarshal(ev.Data, &d) == nil &&
			d["kind"] == capComponent && d["value"] == id.String() && d["restricted"] == restricted {
			n++
		}
	}
	return n
}

// TestPG_Components_A30LiftNeedsAnOrgComponent: lifting a component's restriction is refused (404
// component_not_found, nothing written, no audit row) for the id of a deleted component and for the id
// of a person's row, and works as before for a live org component.
func TestPG_Components_A30LiftNeedsAnOrgComponent(t *testing.T) {
	e := newComponentsPG(t)
	ctx := context.Background()
	lift := func(id uuid.UUID) (int, string) {
		w := e.do(t, http.MethodPut, e.liftPath(id), e.admin, `{"restricted":false}`)
		return w.Code, wireReason(t, w)
	}

	gone := e.putOrg(t, "Deleted")
	if w := e.do(t, http.MethodDelete, "/api/v1/components/"+gone.String(), e.admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	before := e.availabilityWrites(gone, false)
	if code, reason := lift(gone); code != http.StatusNotFound || reason != reasonComponentNotFound {
		t.Errorf("lift of a deleted id = %d %s, want 404 %s", code, reason, reasonComponentNotFound)
	}
	if !e.restricted(t, gone) {
		t.Error("the refused lift removed the deleted id's restriction")
	}

	personal := uuid.New()
	if _, err := e.st.CreateComponent(ctx, types.Component{ID: personal, Owner: "sub-member", Name: "Mine"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetCapabilityRestriction(ctx, capComponent, personal.String(), true, "seed"); err != nil {
		t.Fatal(err)
	}
	if code, reason := lift(personal); code != http.StatusNotFound || reason != reasonComponentNotFound {
		t.Errorf("lift for a person's row id = %d %s, want 404 %s", code, reason, reasonComponentNotFound)
	}
	if !e.restricted(t, personal) {
		t.Error("the refused lift removed the restriction row of a person's row id")
	}
	if got := e.availabilityWrites(gone, false) - before; got != 0 {
		t.Errorf("%d availability audit rows for refused lifts, want 0", got)
	}

	live := e.putOrg(t, "Live")
	if code, reason := lift(live); code != http.StatusOK {
		t.Errorf("lift of a live org id = %d %s, want 200", code, reason)
	}
	if e.restricted(t, live) || e.availabilityWrites(live, false) != 1 {
		t.Errorf("after the lift: restricted = %v, lift audit rows = %d; want open and one row", e.restricted(t, live), e.availabilityWrites(live, false))
	}
}

// TestPG_GovernanceChanges_ComponentLiftNeedsAnOrgComponent: the held lift of a component restriction
// is refused at approval the same way when the component was deleted in between; the restriction stays
// and the change stays pending. A lift of a live component applies.
func TestPG_GovernanceChanges_ComponentLiftNeedsAnOrgComponent(t *testing.T) {
	e := newGovEnv(t)
	ctx := context.Background()
	mk := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := e.pg.CreateRestrictedComponent(ctx, types.Component{ID: id, Name: name}, capComponent, "seed"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	path := func(id uuid.UUID) string { return "/api/v1/permissions/availability/component/" + id.String() }
	restricted := func(id uuid.UUID) bool {
		got, err := e.pg.ListCapabilityRestrictions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got[capComponent][id.String()]
	}

	gone := mk("Held-Deleted")
	ch := e.pending(e.call(e.alice, http.MethodPut, path(gone), `{"restricted":false}`))
	if _, _, _, err := e.pg.DeleteRestrictedComponent(ctx, gone, capComponent, "seed"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the second try proves the change stayed pending
		if w := e.call(e.bob, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusNotFound || wireReason(t, w) != reasonComponentNotFound {
			t.Fatalf("approval %d of a lift for a deleted component = %d %s, want 404 %s", i+1, w.Code, w.Body, reasonComponentNotFound)
		}
	}
	if !restricted(gone) {
		t.Error("the refused held lift removed the deleted id's restriction")
	}
	if rows := e.audits("capability.availability.write"); len(rows) != 0 {
		t.Errorf("%d availability audit rows after a refused held lift, want 0", len(rows))
	}

	live := mk("Held-Live")
	e.approve(e.pending(e.call(e.alice, http.MethodPut, path(live), `{"restricted":false}`)).ID)
	if restricted(live) {
		t.Error("the approved lift of a live org component did not apply")
	}
}

// TestPG_Components_A31CreateClearsGrantsNamingTheID: a grant written for a uuid before any row has
// it is gone once the org component is created, so the new component is attachable by no one.
func TestPG_Components_A31CreateClearsGrantsNamingTheID(t *testing.T) {
	e := newComponentsPG(t)
	id := uuid.New()
	e.grant(t, "user", "sub-member", capComponent, id.String(), "allow")
	if e.grantsNaming(t, id) != 1 {
		t.Fatal("precondition: the early grant is not stored")
	}
	if w := e.do(t, http.MethodPut, "/api/v1/components/"+id.String(), e.admin, saveComponentBody("Early", stripeDef)); w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if n := e.grantsNaming(t, id); n != 0 {
		t.Errorf("%d grants naming the new id survived the create", n)
	}
	if code, body := e.memberDoor(t, id.String()); code != http.StatusForbidden || body != componentRefusalBody {
		t.Errorf("member with the early grant = %d %s, want the refusal bytes", code, body)
	}
	if len(e.memberList(t).Org) != 0 {
		t.Error("the early-granted member sees the new component")
	}
}

// TestPG_Components_A32DeleteAuditsARestoredRestriction: a delete that puts back a lifted restriction
// writes exactly one capability.availability.write row (restricted true); a delete of an id that is
// still restricted writes none.
func TestPG_Components_A32DeleteAuditsARestoredRestriction(t *testing.T) {
	e := newComponentsPG(t)
	del := func(id uuid.UUID) {
		t.Helper()
		if w := e.do(t, http.MethodDelete, "/api/v1/components/"+id.String(), e.admin, ""); w.Code != http.StatusNoContent {
			t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
		}
	}

	closed := e.putOrg(t, "StillRestricted")
	if n := e.availabilityWrites(closed, true); n != 1 {
		t.Fatalf("create wrote %d restricted rows, want 1", n)
	}
	del(closed)
	if n := e.availabilityWrites(closed, true); n != 1 {
		t.Errorf("delete of a still-restricted id: %d restricted rows, want the create's one and none more", n)
	}

	lifted := e.putOrg(t, "Lifted")
	e.liftRestriction(t, lifted)
	del(lifted)
	if n := e.availabilityWrites(lifted, true); n != 2 {
		t.Errorf("delete of a lifted id: %d restricted rows, want the create's and exactly one from the delete", n)
	}
	if d := e.deleteDatum(); d["restriction_kept"] != true {
		t.Errorf("component.delete datum = %v, want restriction_kept true", d)
	}
}
