// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Restrictions (migration 0210) against Postgres: the rows, the caller match, the run column and the
// workspace pin. Every case runs on its own throwaway database.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func restrictionNamed(name string, members ...types.RestrictionMember) types.Restriction {
	deny := []string{name + ".example"}
	return types.Restriction{ID: uuid.New(), Name: name, Spec: types.RestrictionSpec{DeniedDomains: &deny}, Members: members}
}

func userMember(sub string) types.RestrictionMember {
	return types.RestrictionMember{SubjectType: types.CapabilitySubjectUser, Subject: sub}
}

func TestRestrictionListForMatchesTheCallerInNameOrder(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	group := types.RestrictionMember{SubjectType: types.CapabilitySubjectGroup, Subject: "eng"}
	all := types.RestrictionMember{SubjectType: types.CapabilitySubjectAll}
	for _, r := range []types.Restriction{
		restrictionNamed("b-user", userMember("alice")), restrictionNamed("a-group", group),
		restrictionNamed("c-all", all), restrictionNamed("d-other", userMember("bob")),
		restrictionNamed("e-type", types.RestrictionMember{SubjectType: types.CapabilitySubjectUserType, Subject: "contractor"}),
	} {
		if _, err := pg.PutRestriction(ctx, r, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	names := func(rs []types.Restriction) (out []string) {
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return out
	}
	got, err := pg.ListRestrictionsFor(ctx, []string{"alice"}, []string{"eng"}, "contractor")
	if err != nil || len(got) != 4 || got[0].Name != "a-group" || got[1].Name != "b-user" || got[2].Name != "c-all" || got[3].Name != "e-type" {
		t.Fatalf("ListRestrictionsFor = %v, %v; want a-group b-user c-all e-type", names(got), err)
	}
	if len(got[1].Members) != 1 || got[1].Members[0].Subject != "alice" {
		t.Errorf("members not joined: %+v", got[1].Members)
	}
	if got, err = pg.ListRestrictionsFor(ctx, nil, nil, ""); err != nil || len(got) != 1 || got[0].Name != "c-all" {
		t.Errorf("a caller with no subject = %v, %v; want only the all restriction", names(got), err)
	}
	if has, err := pg.HasGroupTierRestrictions(ctx); err != nil || !has {
		t.Errorf("HasGroupTierRestrictions = %v, %v; want true", has, err)
	}
	if n, err := pg.CountRestrictions(ctx); err != nil || n != 5 {
		t.Errorf("CountRestrictions = %d, %v; want 5", n, err)
	}
}

func TestRestrictionPutReplacesMembersAndGetByIDSkipsADeletedOne(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	r := restrictionNamed("contractors", userMember("alice"), userMember("bob"))
	if _, err := pg.PutRestriction(ctx, r, "admin"); err != nil {
		t.Fatal(err)
	}
	r.Members = []types.RestrictionMember{userMember("carol")}
	saved, err := pg.PutRestriction(ctx, r, "admin")
	if err != nil || len(saved.Members) != 1 || saved.Members[0].Subject != "carol" {
		t.Fatalf("replace-set = %+v, %v; want carol alone", saved.Members, err)
	}
	if _, err := pg.PutRestriction(ctx, restrictionNamed("contractors"), "admin"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second restriction named contractors = %v, want ErrConflict", err)
	}
	other := restrictionNamed("other")
	if _, err := pg.PutRestriction(ctx, other, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DeleteRestriction(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetRestrictionsByID(ctx, []uuid.UUID{r.ID, other.ID})
	if err != nil || len(got) != 1 || got[0].ID != other.ID {
		t.Errorf("GetRestrictionsByID after a delete = %+v, %v; want only the survivor", got, err)
	}
	if _, err := pg.DeleteRestriction(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a second delete = %v, want ErrNotFound", err)
	}
}

// The run column round-trips through insert and scan, and through the queued start's restamp.
func TestRestrictionIDsColumn(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	created, other := uuid.New(), uuid.New()

	run := newRun(types.RunPending)
	run.RestrictionIDs = []uuid.UUID{created}
	run, err := pg.CreateRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if got := runState(t, pg, run.ID); len(got.RestrictionIDs) != 1 || got.RestrictionIDs[0] != created {
		t.Fatalf("restriction_ids after insert = %v, want [%s]", got.RestrictionIDs, created)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET admitted_at = now() WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	started, ok, err := pg.StartQueuedRun(ctx, run.ID, store.QueueStart{SPIFFEID: "spiffe://x/start", ConfinementClass: types.CC2,
		RestrictionIDs: []uuid.UUID{created, other}})
	if err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	if len(started.RestrictionIDs) != 2 || runState(t, pg, run.ID).RestrictionIDs[1] != other {
		t.Errorf("restriction_ids after the restamp = %v, want [%s %s]", started.RestrictionIDs, created, other)
	}

	plain, err := pg.CreateRun(ctx, newRun(types.RunPending))
	if err != nil || len(plain.RestrictionIDs) != 0 {
		t.Errorf("a run with no restriction = %v, %v; want an empty set", plain.RestrictionIDs, err)
	}
}

// A workspace pin is written only by SetWorkspaceOwningUnit, read back with the unit's name, and a
// pinned unit cannot be deleted.
func TestOwningUnitPinAndTheDeleteGuard(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	unit, err := pg.UpsertGovernanceUnit(ctx, newGovernanceUnit("platform"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := pg.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: "api", OwnedBy: "alice", Status: types.WorkspaceScanned,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil || ws.OwningUnitID != nil {
		t.Fatalf("a new workspace = %+v, %v; want unpinned", ws.OwningUnitID, err)
	}
	pinned, err := pg.SetWorkspaceOwningUnit(ctx, ws.ID, &unit.ID)
	if err != nil || pinned.OwningUnitID == nil || *pinned.OwningUnitID != unit.ID || pinned.OwningUnitName != "platform" {
		t.Fatalf("pin = %+v, %v; want the unit and its name", pinned, err)
	}
	// UpdateWorkspace never writes the column.
	edited := pinned
	edited.OwningUnitID = nil
	if _, err := pg.UpdateWorkspace(ctx, ws.ID, edited, false); err != nil {
		t.Fatal(err)
	}
	if got, err := pg.GetWorkspace(ctx, ws.ID); err != nil || got.OwningUnitID == nil || got.OwningUnitName != "platform" {
		t.Fatalf("after UpdateWorkspace the pin = %+v, %v; want it kept", got.OwningUnitID, err)
	}
	var pinnedErr *store.ErrUnitPinned
	if err := pg.DeleteGovernanceUnit(ctx, unit.ID); !errors.As(err, &pinnedErr) || len(pinnedErr.Workspaces) != 1 || pinnedErr.Workspaces[0] != "api" {
		t.Fatalf("delete of a pinned unit = %v, want *ErrUnitPinned naming api", err)
	}
	if _, err := pg.SetWorkspaceOwningUnit(ctx, ws.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := pg.DeleteGovernanceUnit(ctx, unit.ID); err != nil {
		t.Errorf("delete after the unpin = %v", err)
	}
	missing := uuid.New()
	if _, err := pg.SetWorkspaceOwningUnit(ctx, ws.ID, &missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("pin to an unknown unit = %v, want ErrNotFound", err)
	}
}
