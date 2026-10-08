// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The components erasure scope and the SCIM purge, over a real Postgres: the person's saved components
// go, the content of the run snapshots they defined goes, and the snapshot rows stay as the content-free
// record that the run carried components. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func erasureComponentDef() types.ComponentDefinition {
	return types.ComponentDefinition{Hosts: []string{"api.example.com"}, Config: map[string]string{"REGION": "eu-west-1"}}
}

// componentFixture is one person's saved component, an organisation component, and a run of the
// person's that launched with both, so its snapshot holds an org row and a self-defined one.
type componentFixture struct {
	saved, org types.Component
	runID      uuid.UUID
}

func seedComponentFixture(t *testing.T, st store.PG, owner string, runID uuid.UUID) componentFixture {
	t.Helper()
	ctx := context.Background()
	f := componentFixture{runID: runID}
	var err error
	if f.saved, err = st.CreateComponent(ctx, types.Component{ID: uuid.New(), Owner: owner, Name: "mine-" + owner, Definition: erasureComponentDef(), CreatedBy: owner}); err != nil {
		t.Fatal(err)
	}
	if f.org, err = st.CreateComponent(ctx, types.Component{ID: uuid.New(), Name: "org-" + owner, Definition: erasureComponentDef(), CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool.Exec(ctx, `DELETE FROM components WHERE id = ANY($1)`, []uuid.UUID{f.saved.ID, f.org.ID})
	})
	if err := st.PutRunComponents(ctx, runID, []types.RunComponent{
		{ComponentID: &f.org.ID, Name: f.org.Name, Version: 1, Definition: erasureComponentDef()},
		{SelfDefined: true, ComponentID: &f.saved.ID, Owner: owner, Name: f.saved.Name, Version: 1, Definition: erasureComponentDef()},
		{SelfDefined: true, Owner: owner, Name: "inline", Definition: erasureComponentDef()},
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// requireErased checks f after owner's erasure: the saved component gone and the organisation's kept,
// the run row intact, the org snapshot row untouched, both self-defined rows left as null-content
// tombstones that still say self-defined, and nothing of owner readable from run_components.
func (f componentFixture) requireErased(t *testing.T, st store.PG, owner string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.GetComponent(ctx, f.saved.ID, owner); err == nil {
		t.Errorf("%s's saved component survived the erasure", owner)
	}
	if _, err := st.GetComponent(ctx, f.org.ID, ""); err != nil {
		t.Errorf("the organisation's component: %v", err)
	}
	if _, err := st.GetRun(ctx, f.runID); err != nil {
		t.Errorf("the run row: %v", err)
	}
	rows, err := st.ListRunComponents(ctx, f.runID)
	if err != nil || len(rows) != 3 {
		t.Fatalf("snapshot = %d rows, %v, want 3 (the rows are never deleted)", len(rows), err)
	}
	if rows[0].Erased || rows[0].SelfDefined || rows[0].Name != f.org.Name || rows[0].ComponentID == nil || *rows[0].ComponentID != f.org.ID {
		t.Errorf("org snapshot row = %+v, want untouched", rows[0])
	}
	for _, r := range rows[1:] {
		if !r.Erased || !r.SelfDefined || r.Owner != "" || r.Name != "" || r.Version != 0 || r.ComponentID != nil || len(r.Definition.Hosts) != 0 || len(r.Definition.Config) != 0 {
			t.Errorf("self-defined snapshot row %d = %+v, want a null-content tombstone", r.Ordinal, r)
		}
	}
	var nulls int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM run_components WHERE run_id = $1 AND owner IS NULL AND name IS NULL
		AND version IS NULL AND definition IS NULL AND component_id IS NULL AND self_defined`, f.runID).Scan(&nulls); err != nil || nulls != 2 {
		t.Errorf("%d tombstone rows with every content column NULL, %v, want 2", nulls, err)
	}
}

// Erasing the components scope for a person: definitions gone, snapshots cleared to tombstones, run
// intact, another person's components and snapshots untouched, and a second erase changes nothing.
func TestPG_PersonErasure_ComponentsScope(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	st := store.NewPG(l.pool)
	const alice, bob = "alice-comp-sub", "bob-comp-sub"
	af := seedComponentFixture(t, st, alice, l.personRun(alice, "alice").ID)
	bf := seedComponentFixture(t, st, bob, l.personRun(bob, "bob").ID)

	for i := 0; i < 2; i++ { // the second pass is the idempotence proof
		w := do(t, rp.srv, http.MethodPost, "/api/v1/people/"+alice+"/erasure", adminToken, erasureBody("components"))
		if w.Code != http.StatusOK {
			t.Fatalf("pass %d: erasure = %d %s", i, w.Code, w.Body)
		}
		var body struct {
			Outcome map[string]string         `json:"outcome"`
			Detail  map[string]map[string]int `json:"detail"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		wantSaved, wantSnaps := 1, 2
		if i == 1 {
			wantSaved, wantSnaps = 0, 0
		}
		if body.Outcome["components"] != "done" || body.Detail["components"]["components"] != wantSaved || body.Detail["components"]["run_snapshots_cleared"] != wantSnaps {
			t.Errorf("pass %d: outcome %v detail %v, want done with %d saved and %d snapshots", i, body.Outcome, body.Detail, wantSaved, wantSnaps)
		}
		af.requireErased(t, st, alice)
	}

	if _, err := st.GetComponent(t.Context(), bf.saved.ID, bob); err != nil {
		t.Errorf("another person's saved component: %v", err)
	}
	rows, err := st.ListRunComponents(t.Context(), bf.runID)
	if err != nil || len(rows) != 3 || rows[1].Erased || rows[1].Owner != bob || rows[2].Erased {
		t.Errorf("another person's snapshot = %+v, %v, want untouched", rows, err)
	}
}

// A8: a components-only erasure, then the feature denied, must leave a revived run refused. The
// erasure half is TestPG_PersonErasure_ComponentsScope; the revive half needs the C7 lane's re-check of
// a run's component rows, which this branch does not have.
func TestPG_PersonErasure_ComponentsOnlyThenDeniedFeatureRefusesRevive(t *testing.T) {
	t.Skip("pending C7: revive re-checks the run_components tombstone doors; run after C7 merges")
}

// A SCIM DELETE erases the same way: the person's components and snapshot content go, the snapshot rows
// stay, the purge row counts the components, and a repeat DELETE changes nothing.
func TestSCIMPurgeErasesComponents(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	cf := seedComponentFixture(t, e.st, purgeSub, e.seedRun(purgeSub, types.RunStopped))
	by := seedComponentFixture(t, e.st, "sub-bystander-comp", e.seedRun("sub-bystander-comp", types.RunStopped))

	for i := 0; i < 2; i++ {
		if w := e.del(e.b, f.id); w.Code != http.StatusNoContent {
			t.Fatalf("DELETE %d = %d %s, want 204", i, w.Code, w.Body)
		}
		cf.requireErased(t, e.st, purgeSub)
	}
	rows := e.purgeRows(e.a, e.b)
	if len(rows) != 1 {
		t.Fatalf("%d purge rows, want 1", len(rows))
	}
	if d := dataOf(t, rows[0]); d["components_erased"] != float64(1) {
		t.Errorf("purge row = %v, want components_erased 1", d)
	}
	if _, err := e.st.GetComponent(t.Context(), by.saved.ID, "sub-bystander-comp"); err != nil {
		t.Errorf("the bystander's saved component: %v", err)
	}
	if got, err := e.st.ListRunComponents(t.Context(), by.runID); err != nil || got[1].Erased {
		t.Errorf("the bystander's snapshot = %+v, %v, want untouched", got, err)
	}
}
