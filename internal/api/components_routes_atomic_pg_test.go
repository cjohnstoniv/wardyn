// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// liftRestriction has the security tier make an org component available to everyone.
func (e componentsPG) liftRestriction(t *testing.T, id uuid.UUID) {
	t.Helper()
	w := e.do(t, http.MethodPut, "/api/v1/permissions/availability/component/"+id.String(), e.admin, `{"restricted":false}`)
	if w.Code != http.StatusOK || e.restricted(t, id) {
		t.Fatalf("lift the restriction = %d %s", w.Code, w.Body.String())
	}
}

// deleteDatum is the last component.delete audit datum, nil when there is none.
func (e componentsPG) deleteDatum() map[string]any {
	var del map[string]any
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "component.delete" {
			del = nil
			_ = json.Unmarshal(ev.Data, &del)
		}
	}
	return del
}

func (e componentsPG) grantsNaming(t *testing.T, id uuid.UUID) int {
	t.Helper()
	grants, err := e.st.ListCapabilityGrants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range grants {
		if g.Capability == capComponent && g.Value == id.String() {
			n++
		}
	}
	return n
}

// TestPG_Components_A11DeleteRestrictsAnIDWhoseRestrictionWasLifted: an org
// component the security tier made available to everyone is deleted. Its id
// ends restricted all the same, so the audit row's restriction_kept is true
// because it is true, and the member who could attach it no longer can.
func TestPG_Components_A11DeleteRestrictsAnIDWhoseRestrictionWasLifted(t *testing.T) {
	e := newComponentsPG(t)
	id := e.putOrg(t, "Lifted")
	e.grant(t, "user", "sub-member", capComponent, id.String(), "allow")
	e.liftRestriction(t, id)
	if code, body := e.memberDoor(t, id.String()); code != 0 {
		t.Fatalf("member before the delete = %d %s, want admitted", code, body)
	}

	if w := e.do(t, http.MethodDelete, "/api/v1/components/"+id.String(), e.admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if !e.restricted(t, id) {
		t.Error("a deleted org component's id is unrestricted (A11)")
	}
	if n := e.grantsNaming(t, id); n != 0 {
		t.Errorf("%d grants naming the deleted id survived", n)
	}
	if code, body := e.memberDoor(t, id.String()); code != http.StatusForbidden || body != componentRefusalBody {
		t.Errorf("member after the delete = %d %s, want the refusal bytes", code, body)
	}
	if del := e.deleteDatum(); del["restriction_kept"] != true || del["grants_removed"] != float64(1) {
		t.Errorf("component.delete datum = %v, want restriction_kept and one grant removed", del)
	}
}

// TestPG_Components_A11FailedDeleteLeavesNothingHalfDone: the grant sweep fails
// inside the delete. The row, the grants and the restriction are all as they
// were and no component.delete row is written, so the retry is a whole delete
// and never a 404 over grants that outlived their component.
func TestPG_Components_A11FailedDeleteLeavesNothingHalfDone(t *testing.T) {
	e := newComponentsPG(t)
	ctx := context.Background()
	id := e.putOrg(t, "Stuck")
	e.grant(t, "user", "sub-member", capComponent, id.String(), "allow")
	e.liftRestriction(t, id)

	for _, ddl := range []string{
		`CREATE FUNCTION c8_refuse_grant_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected sweep failure'; END $$`,
		`CREATE TRIGGER c8_refuse_grant_delete BEFORE DELETE ON capability_grants FOR EACH STATEMENT EXECUTE FUNCTION c8_refuse_grant_delete()`,
	} {
		if _, err := e.st.Pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	if w := e.do(t, http.MethodDelete, "/api/v1/components/"+id.String(), e.admin, ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("delete with a failing sweep = %d %s, want 500", w.Code, w.Body.String())
	}
	if got, err := e.st.GetComponent(ctx, id, ""); err != nil || got.Name != "Stuck" {
		t.Errorf("the row after the failed delete = %+v, %v; want it still there", got, err)
	}
	if n := e.grantsNaming(t, id); n != 1 {
		t.Errorf("grants naming the id after the failed delete = %d, want the one still there", n)
	}
	if e.restricted(t, id) {
		t.Error("the failed delete left its restriction write behind")
	}
	if del := e.deleteDatum(); del != nil {
		t.Errorf("a failed delete wrote a component.delete row: %v", del)
	}

	if _, err := e.st.Pool.Exec(ctx, `DROP TRIGGER c8_refuse_grant_delete ON capability_grants`); err != nil {
		t.Fatal(err)
	}
	if w := e.do(t, http.MethodDelete, "/api/v1/components/"+id.String(), e.admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("the retry = %d %s, want 204", w.Code, w.Body.String())
	}
	if _, err := e.st.GetComponent(ctx, id, ""); err == nil || !e.restricted(t, id) || e.grantsNaming(t, id) != 0 {
		t.Errorf("after the retry: row err = %v, restricted = %v, grants = %d; want gone, restricted, none", err, e.restricted(t, id), e.grantsNaming(t, id))
	}
	if del := e.deleteDatum(); del["restriction_kept"] != true || del["grants_removed"] != float64(1) {
		t.Errorf("component.delete datum after the retry = %v, want restriction_kept and one grant removed", del)
	}
}
