// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// More doors a runner-bound folder passes through: each test fails when the one guard it names is removed.

// A run whose folders are bound to one runner keeps that runner alone, and no place at all when it cannot
// have it: a pin to another runner, a pool without it, a waiting run its own runners refused, or drives
// that mount somewhere else.
func TestBoundFolderTargets_KeepsTheBoundRunnerAlone(t *testing.T) {
	mine, other := uuid.New(), uuid.New()
	own, second, alpha := poolTarget{id: mine.String(), runnerID: &mine}, poolTarget{id: other.String(), runnerID: &other}, poolTarget{id: "alpha", executor: "alpha"}
	mixed := func() map[types.Placement][]poolTarget {
		return map[types.Placement][]poolTarget{types.PlacementLocal: {own, second}, types.PlacementRemote: {alpha}}
	}
	onlyMine := func(kinds map[types.Placement][]poolTarget) bool {
		return len(kinds) == 1 && len(kinds[types.PlacementLocal]) == 1 && *kinds[types.PlacementLocal][0].runnerID == mine
	}
	for name, in := range map[string]placementInput{
		"no pin":                     {req: &createRunRequest{}, mountRunner: &mine},
		"a pin to that runner":       {req: &createRunRequest{RunnerID: mine.String()}, mountRunner: &mine},
		"drives bound to the runner": {req: &createRunRequest{}, mountRunner: &mine, drives: true, driveRunner: &mine},
	} {
		if kinds, refusal := boundFolderTargets(in, mixed()); refusal != nil || !onlyMine(kinds) {
			t.Errorf("%s: %v, %+v; want the bound folder's runner alone", name, kinds, refusal)
		}
	}
	for name, c := range map[string]struct {
		in    placementInput
		kinds map[types.Placement][]poolTarget
	}{
		"a pin to another runner":   {placementInput{req: &createRunRequest{RunnerID: other.String()}, mountRunner: &mine}, mixed()},
		"a pool without the runner": {placementInput{req: &createRunRequest{}, mountRunner: &mine}, map[types.Placement][]poolTarget{types.PlacementLocal: {second}, types.PlacementRemote: {alpha}}},
		"a remote-hosted pool":      {placementInput{req: &createRunRequest{}, mountRunner: &mine}, map[types.Placement][]poolTarget{types.PlacementRemote: {alpha}}},
		"refused on own runners":    {placementInput{req: &createRunRequest{}, mountRunner: &mine, noLocal: true}, mixed()},
		"drives on another runner":  {placementInput{req: &createRunRequest{}, mountRunner: &mine, drives: true, driveRunner: &other}, mixed()},
		"drives on remote runners":  {placementInput{req: &createRunRequest{}, mountRunner: &mine, drives: true}, mixed()},
	} {
		if _, refusal := boundFolderTargets(c.in, c.kinds); refusal == nil || refusal.body.Reason != string(placement.ReasonPlacementLocalPath) {
			t.Errorf("%s: %+v, want %s", name, refusal, placement.ReasonPlacementLocalPath)
		}
	}
	// kindTargets hands such a run to boundFolderTargets: without it the mixed pool keeps every kind.
	kinds, refusal := (&Server{}).kindTargets(context.Background(), placementInput{req: &createRunRequest{}, mountRunner: &mine},
		types.RunnerPool{Name: "Mixed", Scope: types.RunnerPoolUser}, []poolTarget{own, second, alpha})
	if refusal != nil || !onlyMine(kinds) {
		t.Errorf("kindTargets = %v, %+v; want the bound folder's runner alone", kinds, refusal)
	}
}

// Dispatch to a person's runner never binds a folder bound to another runner of theirs: the run fails
// before the sandbox, saying where the folder is.
func TestDispatch_LocalOnAnotherRunnerNeverBindsRunnerBoundFolder(t *testing.T) {
	ro := true
	bound, other := uuid.New(), uuid.New()
	policy := types.RunPolicySpec{MinConfinementClass: types.CC1,
		WorkspaceMounts: []types.WorkspaceMount{{Source: boundFolder, Target: composerWorkspaceTarget, ReadOnly: &ro}}}
	posture := userMountPosture{Roots: []string{boundLaptopRoot}, Sources: map[string]bool{boundFolder: true}, Bound: map[string]uuid.UUID{boundFolder: bound}}
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Placement, run.RunnerID = types.PlacementLocal, &other
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policy, UserMounts: posture,
	})
	if fr.createCalls != 0 {
		t.Errorf("CreateSandbox calls = %d, want 0 for a folder bound to another runner", fr.createCalls)
	}
	ev := findAudit(audit.events, run.ID, "run.dispatch", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), string(placement.ReasonPlacementLocalPath)) {
		t.Errorf("run.dispatch failure = %+v, want reason %s; events=%s", ev, placement.ReasonPlacementLocalPath, auditDump(audit.events, run.ID))
	}
	if hint := st.FailureHint(); !strings.Contains(hint, bound.String()[:8]) || !strings.Contains(hint, "runs only there") {
		t.Errorf("failure hint = %q, want it to name the folder's runner %s", hint, bound.String()[:8])
	}
}

// An edit of a workspace whose binding went stale (its runner was revoked since) is refused with the way
// out: remove runner_id or detach the source.
func TestWorkspaceUpdate_StaleBindingRefusedWithTheWayOut(t *testing.T) {
	assertNoFolderOnThisHost(t)
	srv, st, _ := ownerRunnerHarness(t, runner.UserMountPolicy{Roots: []string{boundLaptopRoot}})
	mine := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)
	body := workspaceBody(t, "laptop-folder", boundSource(boundFolder, mine))
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	created := decodeJSON[types.Workspace](t, w)
	rn := st.rows[mine]
	at := time.Now().UTC()
	rn.State, rn.RevokedAt = types.RunnerRevoked, &at
	st.rows[mine] = rn

	w = doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+created.ID.String(), member, body)
	if w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonWorkspaceSourcesNotAllowed || !strings.Contains(w.Body.String(), "Remove runner_id or detach the source") {
		t.Errorf("an edit over a revoked binding = %d %s, want 400 %s with the way out", w.Code, w.Body.String(), reasonWorkspaceSourcesNotAllowed)
	}
}

// A required write row that widens a bound folder answers for its binding: a supplier whose binding went
// stale refuses the widening by the stale reason, and holders that put the folder in two places refuse it
// as a placement it cannot have.
func TestRequirementWriteRefusal_RunnerBoundFolder(t *testing.T) {
	assertNoFolderOnThisHost(t)
	runners := boundRunners{rows: map[uuid.UUID]types.Runner{}}
	mine := runners.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	revoked := runners.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerRevoked)
	dir := boundLaptopRoot + "/w/app"
	srv := &Server{cfg: Config{
		Store:      boundRefStore{wsRefStore: wsRefStore{}, boundRunners: runners},
		UserMounts: runner.UserMountPolicy{Roots: []string{boundLaptopRoot}, WritableRoots: []string{boundLaptopRoot + "/w"}},
	}}
	rw := false
	mounts := []types.WorkspaceMount{{Source: dir, Target: composerWorkspaceTarget, ReadOnly: &rw}}
	holder := types.Workspace{ID: uuid.New(), Name: "holder", OwnedBy: ownerMemberSub, Sources: []types.WorkspaceSource{boundSource(dir, mine)}}
	stale := types.Workspace{ID: uuid.New(), Name: "stale", OwnedBy: ownerMemberSub, Sources: []types.WorkspaceSource{boundSource(dir, revoked)}}
	unbound := types.Workspace{ID: uuid.New(), Name: "unbound", OwnedBy: ownerMemberSub,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: dir}}}
	ctx := context.Background()

	if r := srv.requirementWriteRefusal(ctx, []types.Workspace{holder}, mounts, []bool{true}, map[int][]types.Workspace{0: {holder}}); r != nil {
		t.Fatalf("a bound folder widened by its own holder = %+v, want it allowed by its text", r)
	}
	r := srv.requirementWriteRefusal(ctx, []types.Workspace{holder}, mounts, []bool{true}, map[int][]types.Workspace{0: {stale}})
	if r == nil || r.status != http.StatusUnprocessableEntity || r.reason != reasonWorkspaceSourceRunnerUnavailable {
		t.Errorf("a supplier whose binding is stale = %+v, want 422 %s", r, reasonWorkspaceSourceRunnerUnavailable)
	}
	r = srv.requirementWriteRefusal(ctx, []types.Workspace{holder, unbound}, mounts, []bool{true}, map[int][]types.Workspace{0: {holder}})
	if r == nil || r.status != http.StatusUnprocessableEntity || r.reason != string(placement.ReasonPlacementLocalPath) {
		t.Errorf("holders that disagree on the folder's place = %+v, want 422 %s", r, placement.ReasonPlacementLocalPath)
	}
}
