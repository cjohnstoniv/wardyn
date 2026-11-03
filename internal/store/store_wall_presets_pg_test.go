// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for wall presets (migration 0248_wall_presets). Guarded by WARDYN_TEST_PG; each
// test works in its own throwaway database.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func pgWallPreset(scope types.WallPresetScope, owner, group, name, by string) types.WallPreset {
	never := types.FirstUseAlwaysDeny
	return types.WallPreset{ID: uuid.NewString(), Scope: scope, OwnerID: owner, GroupID: group, Name: name,
		Spec: types.PersonalWallsSpec{FirstUseApproval: &never}, UpdatedBy: by}
}

// TestPG_WallPresets_WriteRules: a create is version 1; a name is unique per audience in any case, but
// free in another audience; an update at the expected version writes the next and one at a stale
// version writes nothing; the list shows the org's, the person's own and the groups' presets only; a
// group write with no grant writes nothing; delete answers the row it removed.
func TestPG_WallPresets_WriteRules(t *testing.T) {
	st, _ := templatesPG(t)
	ctx := context.Background()
	mine, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopePerson, "alice", "", "Tight", "alice"), nil)
	if err != nil || mine.Version != 1 || mine.OwnerID != "alice" || mine.UpdatedAt == nil || mine.Spec.FirstUseApproval == nil {
		t.Fatalf("create = %+v, %v", mine, err)
	}
	if _, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopePerson, "alice", "", "TIGHT", "alice"), nil); !errors.Is(err, store.ErrTemplateNameTaken) {
		t.Fatalf("same name, other case: err = %v", err)
	}
	if _, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopePerson, "bob", "", "Tight", "bob"), nil); err != nil {
		t.Fatalf("same name for bob: %v", err)
	}
	org, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopeOrg, "", "", "Tight", "root"), nil)
	if err != nil {
		t.Fatal(err)
	}
	grant := &store.TemplateGrantCheck{Group: "eng", Persons: []string{"alice"}}
	if _, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopeGroup, "", "eng", "Eng", "alice"), grant); !errors.Is(err, store.ErrTemplateGrantGone) {
		t.Fatalf("group create with no grant: err = %v", err)
	}
	if _, err := st.PutTemplateGroupAdmin(ctx, types.TemplateGroupAdmin{Group: "eng", Person: "alice", GrantedBy: "root"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopeGroup, "", "eng", "Eng", "alice"), grant); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopeGroup, "", "ops", "Ops", "root"), nil); err != nil {
		t.Fatal(err)
	}
	seen, err := st.ListWallPresetsVisible(ctx, "alice", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, p := range seen {
		names[string(p.Scope)+":"+p.Name]++
	}
	if len(seen) != 3 || names["person:Tight"] != 1 || names["org:Tight"] != 1 || names["group:Eng"] != 1 {
		t.Fatalf("alice sees %v", names)
	}

	mine.Name, mine.UpdatedBy = "Tighter", "alice"
	next, err := st.UpdateWallPreset(ctx, mine, 1, nil)
	if err != nil || next.Version != 2 || next.Name != "Tighter" || next.Scope != types.WallPresetScopePerson || next.OwnerID != "alice" {
		t.Fatalf("update = %+v, %v", next, err)
	}
	var conflict *store.TemplateRevisionConflictError
	if _, err := st.UpdateWallPreset(ctx, mine, 1, nil); !errors.As(err, &conflict) || conflict.Current != 2 {
		t.Fatalf("stale update: err = %v", err)
	}
	if got, _ := st.GetWallPreset(ctx, uuid.MustParse(mine.ID)); got.Version != 2 || got.Name != "Tighter" {
		t.Fatalf("after a stale update = %+v", got)
	}
	gone := pgWallPreset(types.WallPresetScopeOrg, "", "", "x", "root")
	if _, err := st.UpdateWallPreset(ctx, gone, 1, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update of a missing preset: err = %v", err)
	}
	deleted, err := st.DeleteWallPreset(ctx, uuid.MustParse(org.ID), nil)
	if err != nil || deleted.Name != "Tight" || deleted.Scope != types.WallPresetScopeOrg {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	if _, err := st.GetWallPreset(ctx, uuid.MustParse(org.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted preset: err = %v", err)
	}
	if _, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopeBuiltin, "", "", "Mine", "alice"), nil); err == nil {
		t.Fatal("a built-in scope was stored")
	}
}

// TestPG_WallPresets_EraseRunSetups: the run_setups erasure deletes the person's own presets, keeps
// another person's and the organisation's, and blanks the person's name on a shared preset they
// saved. The owner is stored canonical, so a mixed-case email subject is matched. A run launched with
// an erased preset keeps its frozen copy.
func TestPG_WallPresets_EraseRunSetups(t *testing.T) {
	st, pool := templatesPG(t)
	ctx := context.Background()
	alice := types.CanonicalUserSubject("Alice@Example.COM")
	mine, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopePerson, alice, "", "mine", alice), nil)
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopePerson, "bob", "", "bob's", "bob"), nil)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := st.CreateWallPreset(ctx, pgWallPreset(types.WallPresetScopeOrg, "", "", "shared", alice), nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(map[string]any{"id": mine.ID, "name": mine.Name, "scope": mine.Scope, "version": mine.Version, "spec": mine.Spec})
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET wall_preset = $2 WHERE id = $1`, run.ID, frozen); err != nil {
		t.Fatal(err)
	}

	got, err := st.EraseRunSetupsOf(ctx, types.CanonicalUserSubject("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if got.WallPresets != 1 || got.AuthorshipCleared != 1 {
		t.Fatalf("erased = %+v", got)
	}
	if _, err := st.GetWallPreset(ctx, uuid.MustParse(mine.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alice's preset: err = %v", err)
	}
	if p, err := st.GetWallPreset(ctx, uuid.MustParse(bobs.ID)); err != nil || p.UpdatedBy != "bob" {
		t.Fatalf("bob's preset = %+v, %v", p, err)
	}
	p, err := st.GetWallPreset(ctx, uuid.MustParse(shared.ID))
	if err != nil || p.UpdatedBy != "" || p.Name != "shared" {
		t.Fatalf("shared preset = %+v, %v", p, err)
	}
	var createdBy string
	_ = pool.QueryRow(ctx, `SELECT created_by FROM wall_presets WHERE id = $1`, shared.ID).Scan(&createdBy)
	if createdBy != "" {
		t.Fatalf("shared preset created_by = %q", createdBy)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT wall_preset->>'name' FROM agent_runs WHERE id = $1`, run.ID).Scan(&name); err != nil || name != "mine" {
		t.Fatalf("the run's frozen copy = %q, %v", name, err)
	}
}
