// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The workspace owning unit's PG pins (B1): a pinned workspace runs in its unit's walls.

func seedPinnableWorkspace(t *testing.T, e *govEnv, name string) types.Workspace {
	t.Helper()
	ws, err := e.pg.CreateWorkspace(context.Background(), types.Workspace{
		ID: uuid.New(), Name: name, Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed workspace %q: %v", name, err)
	}
	return ws
}

func pinPath(ws types.Workspace) string {
	return "/api/v1/workspaces/" + ws.ID.String() + "/owning-unit"
}

func pinBody(unit *uuid.UUID) string {
	b, _ := json.Marshal(map[string]any{"unit_id": unit})
	return string(b)
}

// TestOwningUnitSelection: a person who is in the pinned unit, above it or below it runs in a context
// that satisfies the pin; one who is only in a sibling is refused workspace_unit_not_yours; two pins on
// different branches refuse everyone below only one of them.
func TestOwningUnitSelection(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	top := treeChild(t, e, `{"name":"os-top","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	owner := treeChild(t, e, `{"name":"os-owner","mode":"pass_through","parent_id":"`+top.ID.String()+`"}`)
	below := treeChild(t, e, `{"name":"os-below","mode":"pass_through","parent_id":"`+owner.ID.String()+`"}`)
	sibling := treeChild(t, e, `{"name":"os-sibling","mode":"pass_through","parent_id":"`+top.ID.String()+`"}`)
	treeMember(t, e, top, types.CapabilitySubjectUser, "sub-os-above", 0, false)
	treeMember(t, e, below, types.CapabilitySubjectUser, "sub-os-below", 0, false)
	treeMember(t, e, sibling, types.CapabilitySubjectUser, "sub-os-sib", 0, false)
	ws := seedPinnableWorkspace(t, e, "os-ws")
	if _, err := e.pg.SetWorkspaceOwningUnit(ctx, ws.ID, &owner.ID); err != nil {
		t.Fatal(err)
	}
	pins, err := e.srv.owningUnitFromRequest(ctx, createRunRequest{WorkspaceID: &ws.ID})
	if err != nil || len(pins) != 1 || pins[0] != owner.ID {
		t.Fatalf("owningUnitFromRequest = %v %v, want [%s]", pins, err, owner.ID)
	}
	run := func(sub string, pins ...uuid.UUID) (governanceCeiling, error) {
		return e.srv.effectiveCeiling(withContextChoice(treeCtx(sub, nil, ""), contextChoice{OwningUnits: pins}))
	}
	if c, err := run("sub-os-above", owner.ID); err != nil || unitIDOf(c) != owner.ID {
		t.Errorf("a member above the pin: unit %s %v, want the owning unit", unitIDOf(c), err)
	}
	if c, err := run("sub-os-below", owner.ID); err != nil || unitIDOf(c) != below.ID {
		t.Errorf("a member below the pin: unit %s %v, want their own unit", unitIDOf(c), err)
	}
	var notYours *errWorkspaceUnitNotYours
	if _, err := run("sub-os-sib", owner.ID); !errors.As(err, &notYours) || notYours.Unit != owner.ID {
		t.Errorf("a sibling-only member: %v, want workspace_unit_not_yours for the owning unit", err)
	}
	// Two pins on different branches: nobody satisfies both unless they sit below the two.
	if _, err := run("sub-os-below", owner.ID, sibling.ID); !errors.As(err, &notYours) {
		t.Errorf("pins on two branches for a member of one: %v, want workspace_unit_not_yours", err)
	}
}

// TestOwningUnitWriteFourEyes: pinning an unpinned workspace and moving the pin to a descendant are
// direct; unpinning and moving off the branch are held under the switch and direct without it. The
// Default unit and an unknown unit are 422.
func TestOwningUnitWriteFourEyes(t *testing.T) {
	e := newGovEnv(t)
	top := treeChild(t, e, `{"name":"ow-top","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	kid := treeChild(t, e, `{"name":"ow-kid","mode":"pass_through","parent_id":"`+top.ID.String()+`"}`)
	other := treeChild(t, e, `{"name":"ow-other","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	ws := seedPinnableWorkspace(t, e, "ow-ws")
	put := func(unit *uuid.UUID) *httptest.ResponseRecorder {
		return e.call(e.alice, http.MethodPut, pinPath(ws), pinBody(unit))
	}
	pinOf := func() *uuid.UUID {
		got, err := e.pg.GetWorkspace(context.Background(), ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.OwningUnitID
	}
	if w := put(&top.ID); w.Code != http.StatusOK || pinOf() == nil || *pinOf() != top.ID {
		t.Fatalf("null -> unit: %d %s, want a direct 200", w.Code, w.Body)
	}
	if w := put(&kid.ID); w.Code != http.StatusOK || *pinOf() != kid.ID {
		t.Fatalf("unit -> descendant: %d %s, want a direct 200", w.Code, w.Body)
	}
	hold := func(unit *uuid.UUID, name string) types.GovernanceChange {
		t.Helper()
		w := put(unit)
		ch := e.pending(w)
		if ch.TargetKind != govKindWorkspaceOwningUnit || ch.TargetKey != ws.ID.String() {
			t.Errorf("%s: held %s/%s", name, ch.TargetKind, ch.TargetKey)
		}
		return ch
	}
	ch := hold(nil, "unpin")
	if got := pinOf(); got == nil || *got != kid.ID {
		t.Fatalf("a held unpin changed the pin to %v", got)
	}
	e.approve(ch.ID)
	if pinOf() != nil {
		t.Errorf("the approved unpin left %v", pinOf())
	}
	put(&top.ID)
	ch = hold(&other.ID, "off-branch move")
	e.approve(ch.ID)
	if got := pinOf(); got == nil || *got != other.ID {
		t.Errorf("the approved move left %v, want %s", got, other.ID)
	}
	if w := put(&store.DefaultUnitID); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonWorkspaceOwningUnitDefault {
		t.Errorf("pinning Default: %d %s, want 422 %s", w.Code, w.Body, reasonWorkspaceOwningUnitDefault)
	}
	unknown := uuid.New()
	if w := put(&unknown); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonGovernanceUnitNotFoundByID {
		t.Errorf("pinning an unknown unit: %d %s, want 422 %s", w.Code, w.Body, reasonGovernanceUnitNotFoundByID)
	}
	if w := e.call(e.alice, http.MethodPut, "/api/v1/workspaces/"+uuid.NewString()+"/owning-unit", pinBody(&top.ID)); w.Code != http.StatusNotFound {
		t.Errorf("an unknown workspace: %d, want 404", w.Code)
	}
	if len(e.audits("workspace.owning_unit.set")) < 4 {
		t.Errorf("audit rows: %d, want one per applied write", len(e.audits("workspace.owning_unit.set")))
	}
	t.Setenv(envGovernanceSecondHuman, "")
	off := newGovEnvOn(t, throwawayPGPool(t))
	u := treeChild(t, off, `{"name":"ow-off","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	ws2 := seedPinnableWorkspace(t, off, "ow-ws2")
	if w := off.call(off.alice, http.MethodPut, pinPath(ws2), pinBody(&u.ID)); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	if w := off.call(off.alice, http.MethodPut, pinPath(ws2), pinBody(nil)); w.Code != http.StatusOK {
		t.Errorf("switch off unpin: %d %s, want direct", w.Code, w.Body)
	}
}

// overlapAtTargetLock runs write while another writer holds governance target kind/key and has made
// edit on its transaction: write reads the state before edit commits, waits at the target lock, and
// returns once edit has committed. Its answer is returned.
func overlapAtTargetLock(t *testing.T, e *govEnv, kind, key string, edit func(q store.Querier) error, write func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	ctx := t.Context()
	other, err := testutil.PGConn(t, e.pool).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) //nolint:errcheck // committed below; a no-op then
	if err := store.LockGovernanceTarget(ctx, other, kind, key); err != nil {
		t.Fatal(err)
	}
	if err := edit(other); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- write() }()
	for deadline := time.Now().Add(15 * time.Second); ; {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
			AND wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		select {
		case w := <-done:
			t.Fatalf("the write answered %d %s without waiting for the other writer", w.Code, w.Body)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the write never waited at the target lock")
		}
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// TestOwningUnitOverlappingMoves: a move judged narrowing on the pin it read is judged again on the
// pin as it stands when it writes. With the workspace pinned to ow-top, a move to ow-kid below it
// overlaps one to ow-other that commits first: from ow-other the move to ow-kid no longer only
// narrows, so it is held, never applied directly, and the pin stays ow-other until it is approved.
func TestOwningUnitOverlappingMoves(t *testing.T) {
	e := newGovEnv(t)
	top := treeChild(t, e, `{"name":"ow-top","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	kid := treeChild(t, e, `{"name":"ow-kid","mode":"pass_through","parent_id":"`+top.ID.String()+`"}`)
	other := treeChild(t, e, `{"name":"ow-other","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	ws := seedPinnableWorkspace(t, e, "ow-ws")
	if w := e.call(e.alice, http.MethodPut, pinPath(ws), pinBody(&top.ID)); w.Code != http.StatusOK {
		t.Fatalf("pin ow-top: %d %s", w.Code, w.Body)
	}
	pinOf := func() *uuid.UUID {
		got, err := e.pg.GetWorkspace(context.Background(), ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.OwningUnitID
	}
	w := overlapAtTargetLock(t, e, govKindWorkspaceOwningUnit, ws.ID.String(),
		func(q store.Querier) error { return store.SetWorkspaceOwningUnitQ(t.Context(), q, ws.ID, &other.ID) },
		func() *httptest.ResponseRecorder {
			return e.call(e.alice, http.MethodPut, pinPath(ws), pinBody(&kid.ID))
		})
	ch := e.pending(w)
	if got := pinOf(); got == nil || *got != other.ID {
		t.Fatalf("the pin is %v after the overlapping move, want ow-other until approval", got)
	}
	e.approve(ch.ID)
	if got := pinOf(); got == nil || *got != kid.ID {
		t.Errorf("the approved move left %v, want ow-kid", got)
	}
}

// TestOwningUnitBlocksUnitDelete: deleting a unit a workspace is pinned to is a 409 governance_unit_in_use
// naming the workspace, and unpinning frees it.
func TestOwningUnitBlocksUnitDelete(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := treeChild(t, e, `{"name":"bd-unit","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	ws := seedPinnableWorkspace(t, e, "bd-workspace")
	if _, err := e.pg.SetWorkspaceOwningUnit(context.Background(), ws.ID, &u.ID); err != nil {
		t.Fatal(err)
	}
	w := e.admin(http.MethodDelete, "/api/v1/governance/units/"+u.ID.String(), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceUnitInUse || !strings.Contains(w.Body.String(), "bd-workspace") {
		t.Fatalf("delete of a pinned unit: %d %s, want 409 %s naming the workspace", w.Code, w.Body, reasonGovernanceUnitInUse)
	}
	if _, err := e.pg.SetWorkspaceOwningUnit(context.Background(), ws.ID, nil); err != nil {
		t.Fatal(err)
	}
	if w := e.admin(http.MethodDelete, "/api/v1/governance/units/"+u.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Errorf("delete after the unpin: %d %s, want 204", w.Code, w.Body)
	}
}

// TestOwningUnitQueued: a queued launch on a pinned workspace starts in the owning unit, not the home
// unit the person would run in with no workspace.
func TestOwningUnitQueued(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	top := treeChild(t, e, `{"name":"oq-top","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	home := treeChild(t, e, `{"name":"oq-home","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	treeMember(t, e, top, types.CapabilitySubjectUser, "sub-oq", 0, false)
	treeMember(t, e, home, types.CapabilitySubjectUser, "sub-oq", 0, true)
	ws := seedPinnableWorkspace(t, e, "oq-ws")
	if _, err := e.pg.SetWorkspaceOwningUnit(context.Background(), ws.ID, &top.ID); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(queueAuthority{Sub: "sub-oq", Role: "user", UserType: types.UserTypeStandard, Groups: []string{}})
	req := createRunRequest{Agent: "claude-code", WorkspaceID: &ws.ID}
	rawReq, _ := json.Marshal(req)
	raw, _ := json.Marshal(queuedLaunch{Request: rawReq, UnitID: &top.ID, OwningUnits: []uuid.UUID{top.ID}})
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: "sub-oq"}
	st, err := newQueuedStart(context.Background(), context.Background(), store.PromotedRun{Run: run, Entry: store.RunQueueEntry{Authority: auth, Launch: raw}})
	if err != nil {
		t.Fatal(err)
	}
	if c := treeCeiling(t, e, treeCtx("sub-oq", nil, "")); unitIDOf(c) != home.ID {
		t.Fatalf("without the pin the person runs in %s, want their home unit", unitIDOf(c))
	}
	c, end := e.srv.queueGovernance(st, req)
	if end != nil || unitIDOf(c) != top.ID {
		t.Errorf("queued start on a pinned workspace: unit %s end %+v, want the owning unit", unitIDOf(c), end)
	}
}
