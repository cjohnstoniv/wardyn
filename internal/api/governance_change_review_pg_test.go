// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func impactPath(id uuid.UUID) string { return "/api/v1/governance/changes/" + id.String() + "/impact" }

// readImpact is a 200 impact, failing the test on any other status.
func readImpact(t *testing.T, w *httptest.ResponseRecorder) changeImpact {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("impact = %d %s, want 200", w.Code, w.Body)
	}
	var out changeImpact
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("impact body %s: %v", w.Body, err)
	}
	return out
}

// storedReview is the review a change's stored diff holds, read from the database.
func storedReview(t *testing.T, e *govEnv, id uuid.UUID) *changeReview {
	t.Helper()
	ch, err := e.pg.GetGovernanceChange(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var d governanceDiffBody
	if err := json.Unmarshal(ch.Diff, &d); err != nil {
		t.Fatalf("stored diff %s: %v", ch.Diff, err)
	}
	return d.Review
}

func reviewNames(r *changeReview, field string) bool {
	return r != nil && slices.ContainsFunc(r.Widens, func(w composer.Widening) bool { return w.Field == field })
}

// TestPG_ChangeImpact_ProfileUpdate: a unit admin's held widening of a unit shows the person whose walls
// change and the live run there with its owner, a title only for a reader who may see it, then stale once
// the unit is written around it. A unit admin who cannot see the change gets 404, and no impact read
// writes an audit row.
func TestPG_ChangeImpact_ProfileUpdate(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	ctx := context.Background()
	person := treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, "sub-imp-person", 1, false)
	runID := uuid.New()
	if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: runID, CreatedBy: "sub-imp-runner", Agent: "claude-code", ConfinementClass: types.CC2,
		State: types.RunRunning, RunnerTarget: "docker", Task: "t", Title: "quarterly close", SPIFFEID: "spiffe://wardyn.local/agent-run/" + runID.String(),
		SandboxRef: "wardyn-agent-" + runID.String(), UnitID: &e.a1.ID}); err != nil {
		t.Fatal(err)
	}
	ch := e.pending(e.call(e.adminA, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(),
		uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org", "github.com", "npmjs.org"), nil)))
	if r := storedReview(t, e.govEnv, ch.ID); r == nil || r.Verdict != verdictWidens || !reviewNames(r, "allowed_domains") || r.Risk == nil || r.Risk.Before == nil {
		t.Errorf("stored review = %+v, want widens on allowed_domains with a risk before and after", r)
	}
	audits := len(e.h.audit.snapshot())

	w := e.call(e.bob, http.MethodGet, impactPath(ch.ID), "")
	got := readImpact(t, w)
	if got.Computed != impactTree || got.People == nil || got.LiveRuns == nil || got.LiveRunsTotal == nil || *got.LiveRunsTotal != 1 {
		t.Fatalf("impact = %s, want a tree impact with one live run", w.Body)
	}
	if !slices.ContainsFunc(*got.People, func(p treeDryRunPerson) bool { return p.MembershipID == person.ID }) {
		t.Errorf("people = %+v, want membership %s", *got.People, person.ID)
	}
	if runs := *got.LiveRuns; len(runs) != 1 || runs[0].ID != runID || runs[0].Owner != "sub-imp-runner" || runs[0].Title != "quarterly close" || runs[0].StartedAt.IsZero() {
		t.Errorf("live_runs = %+v, want the run with its owner, start and (for a security admin) its title", runs)
	}

	w = e.call(e.peerA, http.MethodGet, impactPath(ch.ID), "")
	got = readImpact(t, w)
	if runs := *got.LiveRuns; len(runs) != 1 || runs[0].Owner != "sub-imp-runner" || runs[0].Title != "" {
		t.Errorf("a unit admin's live_runs = %+v, want the owner and no title", runs)
	}
	if strings.Contains(w.Body.String(), "quarterly close") {
		t.Errorf("a reader below security who does not own the run read its title: %s", w.Body)
	}
	if w := e.call(e.adminB, http.MethodGet, impactPath(ch.ID), ""); w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeNotFound {
		t.Errorf("a unit admin of another unit = %d %s, want 404", w.Code, w.Body)
	}
	if n := len(e.h.audit.snapshot()); n != audits {
		t.Errorf("impact reads wrote %d audit rows, want none", n-audits)
	}

	if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org"), nil)); w.Code != http.StatusOK {
		t.Fatalf("the admin token's write = %d %s", w.Code, w.Body)
	}
	w = e.call(e.bob, http.MethodGet, impactPath(ch.ID), "")
	if got := readImpact(t, w); got.Computed != impactNone || got.NotComputed != impactStaleNotComputed || got.People != nil {
		t.Errorf("after a write to the target, impact = %s, want none/stale", w.Body)
	}
}

// TestPG_ChangeImpact_NotPendingAndBoundary: an applied change answers 409, and a held boundary change
// answers the boundary dry run.
func TestPG_ChangeImpact_NotPendingAndBoundary(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	held := e.heldOn(e.a2)
	e.approve(held.ID)
	if w := e.call(e.bob, http.MethodGet, impactPath(held.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeNotPending {
		t.Errorf("an applied change = %d %s, want 409 governance_change_not_pending", w.Code, w.Body)
	}

	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/boundary", boundaryJSON(testBoundary(types.BoundaryHardLimits))))
	if r := storedReview(t, e.govEnv, ch.ID); r == nil || r.Verdict != verdictNotChecked {
		t.Errorf("a boundary change's review = %+v, want not_checked", r)
	}
	w := e.call(e.bob, http.MethodGet, impactPath(ch.ID), "")
	got := readImpact(t, w)
	if got.Computed != impactBoundary || got.Boundary == nil || got.Boundary.Target != dryRunTargetBoundary || got.People != nil {
		t.Errorf("a boundary impact = %s, want computed boundary with the boundary report", w.Body)
	}
}

// TestPG_ChangeImpact_KindWithNoDryRun: a role mapping change answers none/kind to a super admin, and a
// security admin, who may not see it, gets 404.
func TestPG_ChangeImpact_KindWithNoDryRun(t *testing.T) {
	e := newRoleMapEnv(t, true, govRoleMapChart)
	ch := e.pending(e.call(e.proposer, http.MethodPost, "/api/v1/access/mappings", `{"value":"Eng","role":"user"}`))
	w := e.call(e.approver, http.MethodGet, impactPath(ch.ID), "")
	if got := readImpact(t, w); got.Computed != impactNone || got.NotComputed != impactKindNotComputed || got.Boundary != nil {
		t.Errorf("a role mapping impact = %s, want none/kind", w.Body)
	}
	if w := e.call(e.sec, http.MethodGet, impactPath(ch.ID), ""); w.Code != http.StatusNotFound {
		t.Errorf("a security admin reading a role mapping impact = %d, want 404", w.Code)
	}
}

// TestPG_ChangeImpact_MembershipAddAndDelete: a held membership add and a held membership delete,
// which the tree dry run does not model, answer none/kind, never a tree with no people.
func TestPG_ChangeImpact_MembershipAddAndDelete(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	for name, ch := range map[string]types.GovernanceChange{"add": e.heldOn(e.a2), "delete": e.legacyDelete("sub-imp-legacy", e.a2)} {
		if want := map[string]string{"add": "upsert", "delete": "delete"}[name]; ch.TargetKind != govKindMembership || ch.Op != want {
			t.Fatalf("the held %s = %s/%s, want %s/%s", name, ch.TargetKind, ch.Op, govKindMembership, want)
		}
		w := e.call(e.bob, http.MethodGet, impactPath(ch.ID), "")
		got := readImpact(t, w)
		if got.Computed == impactTree || got.Computed != impactNone || got.NotComputed != impactKindNotComputed {
			t.Errorf("a held membership %s's impact = %s, want none/kind", name, w.Body)
		}
		if got.People != nil || got.LiveRuns != nil || got.LiveRunsTotal != nil || strings.Contains(w.Body.String(), `"people"`) {
			t.Errorf("a held membership %s's impact claims people or live runs: %s", name, w.Body)
		}
	}
}

// TestPG_ChangeReview_EachCheckedKind: a held change of each checked kind stores the verdict its check
// found, through the real routes.
func TestPG_ChangeReview_EachCheckedKind(t *testing.T) {
	e := newUnitAdminEnv(t, true)

	// A profile create: not_checked, with a risk whose before is none.
	created := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/units", uaUnitBody("rv-new", store.DefaultUnitID, uaDomains("pypi.org"), nil)))
	if r := storedReview(t, e.govEnv, created.ID); r == nil || r.Verdict != verdictNotChecked || r.Risk == nil || r.Risk.Before != nil {
		t.Errorf("a create's review = %+v, want not_checked with risk after only", r)
	}

	// A unit admin's move to wider walls: widens, graded source against destination. A security admin's
	// move: not_checked.
	m := treeMember(t, e.govEnv, e.a2, types.CapabilitySubjectUser, "sub-rv-mover", 1, false)
	moved := e.pending(e.call(e.adminA, http.MethodPut, "/api/v1/governance/memberships/"+m.ID.String(), `{"unit_id":"`+e.a1.ID.String()+`"}`))
	if r := storedReview(t, e.govEnv, moved.ID); r == nil || r.Verdict != verdictWidens || !reviewNames(r, "allowed_domains") || r.Risk == nil || r.Risk.Before == nil {
		t.Errorf("a unit admin's widening move = %+v, want widens with risk", r)
	}
	m2 := treeMember(t, e.govEnv, e.a2, types.CapabilitySubjectUser, "sub-rv-mover-2", 1, false)
	secMove := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/memberships/"+m2.ID.String(), `{"unit_id":"`+e.a1.ID.String()+`"}`))
	if r := storedReview(t, e.govEnv, secMove.ID); r == nil || r.Verdict != verdictNotChecked || r.Risk != nil {
		t.Errorf("a security admin's move = %+v, want not_checked", r)
	}

	// A restriction created with an allowlist, and one loosened.
	allow := types.RestrictionWrite{Name: "rv-allow", Spec: types.RestrictionSpec{AllowedDomains: strs("b.example")}, Members: []types.RestrictionMember{userMember("sub-rv")}}
	first := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/restrictions", restrictionJSON(t, allow)))
	if r := storedReview(t, e.govEnv, first.ID); r == nil || r.Verdict != verdictNarrowsHeld || !slices.Equal(r.HeldBecause, []string{heldFirstAllowlist}) {
		t.Errorf("a create naming an allowlist = %+v, want narrows_held first_allowlist", r)
	}
	deny := types.RestrictionWrite{Name: "rv-deny", Spec: types.RestrictionSpec{DeniedDomains: strs("a.example")}, Members: []types.RestrictionMember{userMember("sub-rv")}}
	w := e.call(e.alice, http.MethodPost, "/api/v1/governance/restrictions", restrictionJSON(t, deny))
	var r types.Restriction
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("restriction create = %d %s", w.Code, w.Body)
	}
	deny.Spec.DeniedDomains = nil
	loose := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/restrictions/"+r.ID.String(), restrictionJSON(t, deny)))
	if rv := storedReview(t, e.govEnv, loose.ID); rv == nil || rv.Verdict != verdictWidens || !reviewNames(rv, "denied_domains") {
		t.Errorf("a dropped deny = %+v, want widens on denied_domains", rv)
	}
}

// TestPG_ChangeReview_PinMoves: a held workspace pin move and a held component pin move store widens
// owning_unit.
func TestPG_ChangeReview_PinMoves(t *testing.T) {
	e := newGovEnv(t)
	top := treeChild(t, e, `{"name":"rv-top","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	other := treeChild(t, e, `{"name":"rv-other","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	ws := seedPinnableWorkspace(t, e, "rv-ws")
	if w := e.call(e.alice, http.MethodPut, pinPath(ws), pinBody(&top.ID)); w.Code != http.StatusOK {
		t.Fatalf("first pin = %d %s", w.Code, w.Body)
	}
	wsPin := e.pending(e.call(e.alice, http.MethodPut, pinPath(ws), pinBody(&other.ID)))
	u := seedCPTree(t, e, "rv")
	c := seedOrgComponent(t, e, "rv-tool")
	pinComponent(t, e, c.ID, &u.eu.ID)
	cPin := e.pending(e.call(e.alice, http.MethodPut, componentPinPath(c.ID), pinBody(&u.dev.ID)))
	for _, id := range []uuid.UUID{wsPin.ID, cPin.ID} {
		if rv := storedReview(t, e, id); rv == nil || rv.Verdict != verdictWidens || !reviewNames(rv, wideningOwningUnit) {
			t.Errorf("a pin move = %+v, want widens owning_unit", rv)
		}
	}
}

// TestPG_ChangeReview_NoMountSourceInTheReview: a super admin's held mount add stores a review naming the
// mounts widening and the mount's target, never its host path, which the full-tier diff around it holds.
func TestPG_ChangeReview_NoMountSourceInTheReview(t *testing.T) {
	e := newMountEnv(t, true)
	p := e.seedMountProfile("rv-mnt", pinMounts())
	added := types.WorkspaceMount{Source: pinMountNew, Target: "/home/agent/pin-c"}
	sv, _ := e.view(e.super, p.ID)
	ch := e.pending(e.putProfile(e.super, p.ID, standaloneBody(sv, append(slices.Clone(sv.Ceiling.WorkspaceMounts), added))))
	stored, err := e.pg.GetGovernanceChange(context.Background(), ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Diff), pinMountNew) {
		t.Fatalf("the stored diff does not hold the host path, so its absence from the review proves nothing: %s", stored.Diff)
	}
	review := mustJSON(storedReview(t, e, ch.ID))
	if strings.Contains(string(review), pinMountNew) || strings.Contains(string(review), pinMountSrc) || !strings.Contains(string(review), `"workspace_mounts"`) ||
		!strings.Contains(string(review), added.Target) {
		t.Errorf("stored review = %s, want the mounts widening and its target, never a host path", review)
	}
}
