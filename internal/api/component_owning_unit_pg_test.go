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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The context pins' PG pins: an organisation component pinned to a unit, and a strict workspace pin.

// cpTree is Root > Prod > ProdEU and Dev, written as an admin writes them.
type cpTree struct{ root, prod, eu, dev types.GovernanceUnit }

func seedCPTree(t *testing.T, e *govEnv, prefix string) cpTree {
	t.Helper()
	var c cpTree
	c.root = treeChild(t, e, `{"name":"`+prefix+`-root","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	c.prod = treeChild(t, e, `{"name":"`+prefix+`-prod","mode":"pass_through","parent_id":"`+c.root.ID.String()+`"}`)
	c.eu = treeChild(t, e, `{"name":"`+prefix+`-eu","mode":"pass_through","parent_id":"`+c.prod.ID.String()+`"}`)
	c.dev = treeChild(t, e, `{"name":"`+prefix+`-dev","mode":"pass_through","parent_id":"`+c.root.ID.String()+`"}`)
	return c
}

// seedOrgComponent writes an organisation component restricted to nobody until granted, then granted
// to each of subs, as the admin routes leave it.
func seedOrgComponent(t *testing.T, e *govEnv, name string, subs ...string) types.Component {
	t.Helper()
	ctx := context.Background()
	c, err := e.pg.CreateComponent(ctx, types.Component{ID: uuid.New(), Name: name, CreatedBy: "admin",
		Definition: types.ComponentDefinition{Hosts: []string{"org-api.example"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pg.SetCapabilityRestriction(ctx, capComponent, c.ID.String(), true, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, sub := range subs {
		if _, err := e.pg.UpsertCapabilityGrant(ctx, grant(types.CapabilitySubjectUser, sub, capComponent, c.ID.String(), types.CapabilityAllow)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func pinComponent(t *testing.T, e *govEnv, id uuid.UUID, unit *uuid.UUID) {
	t.Helper()
	if err := store.SetComponentOwningUnitQ(context.Background(), e.pool, id, unit); err != nil {
		t.Fatal(err)
	}
}

func componentPinPath(id uuid.UUID) string {
	return "/api/v1/components/" + id.String() + "/owning-unit"
}

func componentPinOf(t *testing.T, e *govEnv, id uuid.UUID) *uuid.UUID {
	t.Helper()
	got, err := e.pg.GetComponent(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	return got.OwningUnitID
}

// attachChoice is the context choice of a launch attaching component id, as sub.
func attachChoice(t *testing.T, e *govEnv, sub string, id uuid.UUID) (context.Context, contextChoice) {
	t.Helper()
	ctx := treeCtx(sub, nil, "")
	choice, err := e.srv.contextChoiceForRequest(ctx, createRunRequest{Components: []types.ComponentRef{{ID: &id}}})
	if err != nil {
		t.Fatalf("context choice for %s: %v", sub, err)
	}
	return ctx, choice
}

// TestComponentPinRefusesOutsideUnitPG: on the real store, a granted Dev member attaching a component
// pinned to Prod is refused component_unit_pinned naming Prod; a granted ProdEU member runs in ProdEU.
// Its no-PG twin is TestComponentPinRefusesOutsideUnit.
func TestComponentPinRefusesOutsideUnitPG(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := seedCPTree(t, e, "cpr")
	treeMember(t, e, u.dev, types.CapabilitySubjectUser, "sub-cpr-dev", 0, false)
	treeMember(t, e, u.eu, types.CapabilitySubjectUser, "sub-cpr-eu", 0, false)
	treeMember(t, e, u.root, types.CapabilitySubjectUser, "sub-cpr-root", 0, false)
	c := seedOrgComponent(t, e, "cpr-tool", "sub-cpr-dev", "sub-cpr-eu", "sub-cpr-root")
	pinComponent(t, e, c.ID, &u.prod.ID)

	ctx, choice := attachChoice(t, e, "sub-cpr-dev", c.ID)
	if len(choice.StrictPins) != 1 || choice.StrictPins[0].Unit != u.prod.ID || choice.StrictPins[0].Component != "cpr-tool" {
		t.Fatalf("the Dev member's strict pins = %+v, want the component's Prod pin", choice.StrictPins)
	}
	_, err := e.srv.effectiveCeiling(withContextChoice(ctx, choice))
	ref := contextRunRefusal(err)
	if ref == nil || ref.status != http.StatusForbidden || ref.body.Reason != reasonComponentUnitPinned || !strings.Contains(ref.body.Error, "cpr-prod") {
		t.Fatalf("a Dev member: %v (%+v), want 403 %s naming cpr-prod", err, ref, reasonComponentUnitPinned)
	}
	ctx, choice = attachChoice(t, e, "sub-cpr-eu", c.ID)
	if c, err := e.srv.effectiveCeiling(withContextChoice(ctx, choice)); err != nil || unitIDOf(c) != u.eu.ID {
		t.Errorf("a ProdEU member: unit %s %v, want ProdEU", unitIDOf(c), err)
	}
	// TestComponentPinStrictAgainstAncestor, on the real store: Root is above the pin, and refused.
	ctx, choice = attachChoice(t, e, "sub-cpr-root", c.ID)
	if _, err := e.srv.effectiveCeiling(withContextChoice(ctx, choice)); contextRunRefusal(err) == nil {
		t.Errorf("a Root-only member: %v, want refused (no ancestor mapping)", err)
	}
}

// TestComponentPinNoOracleWhenNotGranted: a member not granted a pinned component carries no pin, so
// they get the attach door's refusal, which does not name the pin's unit.
func TestComponentPinNoOracleWhenNotGranted(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := seedCPTree(t, e, "cpo")
	treeMember(t, e, u.dev, types.CapabilitySubjectUser, "sub-cpo-other", 0, false)
	c := seedOrgComponent(t, e, "cpo-tool")
	pinComponent(t, e, c.ID, &u.prod.ID)
	_, choice := attachChoice(t, e, "sub-cpo-other", c.ID)
	if len(choice.StrictPins) != 0 {
		t.Fatalf("an ungranted member's strict pins = %+v, want none", choice.StrictPins)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(treeCtx("sub-cpo-other", nil, ""))
	ref := e.srv.componentAttachRefusal(r, c.ID.String())
	if ref == nil {
		t.Fatal("the attach door admitted an ungranted member")
	}
	w := httptest.NewRecorder()
	ref.write(e.srv, w, r)
	if w.Code != http.StatusForbidden || wireReason(t, w) != string(authz.ReasonCapabilityComponent) {
		t.Errorf("the door's refusal = %d %s, want the component capability refusal", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "cpo-prod") {
		t.Errorf("the refusal %s names the pin's unit", w.Body)
	}
}

// TestComponentSignInNotPinned: a person's own CLI sign-in asks only the attach door, which a granted
// member outside the pinned unit passes: the sign-in is not a run and has no context.
func TestComponentSignInNotPinned(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := seedCPTree(t, e, "cps")
	treeMember(t, e, u.dev, types.CapabilitySubjectUser, "sub-cps-dev", 0, false)
	c := seedOrgComponent(t, e, "cps-tool", "sub-cps-dev")
	pinComponent(t, e, c.ID, &u.prod.ID)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/components/"+c.ID.String()+"/sign-in", nil).WithContext(treeCtx("sub-cps-dev", nil, ""))
	if ref := e.srv.componentAttachRefusal(r, c.ID.String()); ref != nil {
		t.Errorf("the sign-in's door refused a granted member for the pin: %+v", ref)
	}
}

// TestComponentPinSurvivesDefinitionEdit: a definition edit (the store write PUT /components/{id}
// makes) keeps the pin and moves the version by one; the pin write does not move it.
func TestComponentPinSurvivesDefinitionEdit(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := seedCPTree(t, e, "cpd")
	c := seedOrgComponent(t, e, "cpd-tool")
	pinComponent(t, e, c.ID, &u.prod.ID)
	pinned, err := e.pg.GetComponent(context.Background(), c.ID, "")
	if err != nil || pinned.Version != c.Version || pinned.OwningUnitName != "cpd-prod" {
		t.Fatalf("after the pin: version %d name %q %v, want version %d and the unit's name", pinned.Version, pinned.OwningUnitName, err, c.Version)
	}
	pinned.Definition.Hosts = []string{"org-api-2.example"}
	edited, err := e.pg.UpdateComponent(context.Background(), pinned, 0)
	if err != nil {
		t.Fatal(err)
	}
	if edited.OwningUnitID == nil || *edited.OwningUnitID != u.prod.ID || edited.Version != c.Version+1 {
		t.Errorf("after the edit: pin %v version %d, want cpd-prod and version %d", edited.OwningUnitID, edited.Version, c.Version+1)
	}
}

// TestSetComponentOwningUnitRoute: pinning is direct; moving up or unpinning is held under the switch
// and applied on approval; Default and an unknown unit are 422; a person's row and an unknown id 404;
// the admin token writes with a bypass row; a unit admin is refused at the tier.
func TestSetComponentOwningUnitRoute(t *testing.T) {
	e := newGovEnv(t)
	u := seedCPTree(t, e, "cpw")
	c := seedOrgComponent(t, e, "cpw-tool")
	put := func(unit *uuid.UUID) *httptest.ResponseRecorder {
		return e.call(e.alice, http.MethodPut, componentPinPath(c.ID), pinBody(unit))
	}
	if w := put(&u.prod.ID); w.Code != http.StatusOK {
		t.Fatalf("null -> Prod: %d %s, want a direct 200", w.Code, w.Body)
	}
	rows := e.audits(componentOwningUnitAction)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"from":null`) || !strings.Contains(string(rows[0].Data), u.prod.ID.String()) {
		t.Fatalf("audit rows = %+v, want one {from: null, to: Prod}", rows)
	}
	if w := put(&u.eu.ID); w.Code != http.StatusOK {
		t.Fatalf("Prod -> ProdEU: %d %s, want a direct 200", w.Code, w.Body)
	}
	for _, move := range []struct {
		name string
		unit *uuid.UUID
	}{{"up to Root", &u.root.ID}, {"unpin", nil}} {
		ch := e.pending(put(move.unit))
		if ch.TargetKind != govKindComponentOwningUnit || ch.TargetKey != c.ID.String() {
			t.Fatalf("%s: held %s/%s", move.name, ch.TargetKind, ch.TargetKey)
		}
		if got := componentPinOf(t, e, c.ID); got == nil || *got != u.eu.ID {
			t.Fatalf("%s: the held move changed the pin to %v", move.name, got)
		}
		e.approve(ch.ID)
		got := componentPinOf(t, e, c.ID)
		if (move.unit == nil) != (got == nil) || (got != nil && *got != *move.unit) {
			t.Fatalf("%s: the approved move left %v", move.name, got)
		}
		pinComponent(t, e, c.ID, &u.eu.ID)
	}
	if w := put(&store.DefaultUnitID); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonComponentOwningUnitDefault {
		t.Errorf("Default: %d %s, want 422 %s", w.Code, w.Body, reasonComponentOwningUnitDefault)
	}
	unknown := uuid.New()
	if w := put(&unknown); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonGovernanceUnitNotFoundByID {
		t.Errorf("an unknown unit: %d %s, want 422 %s", w.Code, w.Body, reasonGovernanceUnitNotFoundByID)
	}
	mine, err := e.pg.CreateComponent(context.Background(), types.Component{ID: uuid.New(), Owner: "sub-cpw-member", Name: "cpw-mine"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{mine.ID, uuid.New()} {
		if w := e.call(e.alice, http.MethodPut, componentPinPath(id), pinBody(&u.prod.ID)); w.Code != http.StatusNotFound || wireReason(t, w) != reasonComponentNotFound {
			t.Errorf("a person's row or an unknown id: %d %s, want 404 %s", w.Code, w.Body, reasonComponentNotFound)
		}
	}
	if w := e.call(e.user, http.MethodPut, componentPinPath(c.ID), pinBody(&u.prod.ID)); w.Code != http.StatusForbidden {
		t.Errorf("a member: %d, want 403 at the tier", w.Code)
	}
	if w := e.admin(http.MethodPut, componentPinPath(c.ID), pinBody(nil)); w.Code != http.StatusOK || componentPinOf(t, e, c.ID) != nil {
		t.Fatalf("the admin token's unpin: %d %s, want applied at once", w.Code, w.Body)
	}
	if len(e.audits("governance.change.bypass")) == 0 {
		t.Error("the admin token's write left no governance bypass row")
	}
	ua := newUnitAdminEnv(t, false)
	if w := ua.call(ua.adminA, http.MethodPut, componentPinPath(uuid.New()), pinBody(&ua.a.ID)); w.Code != http.StatusForbidden {
		t.Errorf("a unit admin: %d %s, want 403 at the tier", w.Code, w.Body)
	}
}

// TestComponentPinHeldChangeStale: a pin moved after a held move was proposed makes its decide stale.
func TestComponentPinHeldChangeStale(t *testing.T) {
	e := newGovEnv(t)
	u := seedCPTree(t, e, "cpt")
	c := seedOrgComponent(t, e, "cpt-tool")
	pinComponent(t, e, c.ID, &u.eu.ID)
	ch := e.pending(e.call(e.alice, http.MethodPut, componentPinPath(c.ID), pinBody(&u.dev.ID)))
	pinComponent(t, e, c.ID, &u.prod.ID)
	w := e.call(e.bob, http.MethodPost, "/api/v1/governance/changes/"+ch.ID.String()+"/approve", "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Errorf("approving after the pin moved: %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if got := componentPinOf(t, e, c.ID); got == nil || *got != u.prod.ID {
		t.Errorf("the stale decide left the pin %v, want cpt-prod", got)
	}
}

func strictPinBody(unit *uuid.UUID, strict *bool) string {
	m := map[string]any{"unit_id": unit}
	if strict != nil {
		m["strict"] = *strict
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// TestWorkspaceStrictPin: making a pin strict is direct and audited; a Root member is then refused
// with the strict text; making it plain is held; strict with no unit is 422; a move down with strict
// absent keeps it strict.
func TestWorkspaceStrictPin(t *testing.T) {
	e := newGovEnv(t)
	u := seedCPTree(t, e, "cws")
	treeMember(t, e, u.root, types.CapabilitySubjectUser, "sub-cws-root", 0, false)
	ws := seedPinnableWorkspace(t, e, "cws-ws")
	on, off := true, false
	put := func(unit *uuid.UUID, strict *bool) *httptest.ResponseRecorder {
		return e.call(e.alice, http.MethodPut, pinPath(ws), strictPinBody(unit, strict))
	}
	read := func() types.Workspace {
		got, err := e.pg.GetWorkspace(context.Background(), ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if w := put(&u.prod.ID, nil); w.Code != http.StatusOK || read().OwningUnitStrict {
		t.Fatalf("a plain pin: %d %s strict %v", w.Code, w.Body, read().OwningUnitStrict)
	}
	if w := put(&u.prod.ID, &on); w.Code != http.StatusOK || !read().OwningUnitStrict {
		t.Fatalf("strict: true: %d %s, want a direct 200 and a strict row", w.Code, w.Body)
	}
	rows := e.audits(workspaceOwningUnitAction)
	if len(rows) == 0 || !strings.Contains(string(rows[len(rows)-1].Data), `"strict":true`) {
		t.Errorf("the strict write's audit rows = %+v, want strict: true", rows)
	}
	plain, strict, err := e.srv.owningUnitFromRequest(treeCtx("sub-cws-root", nil, ""), createRunRequest{WorkspaceID: &ws.ID})
	if err != nil || len(plain) != 0 || len(strict) != 1 || strict[0].Unit != u.prod.ID {
		t.Fatalf("the request's pins = %v %+v %v, want one strict Prod pin", plain, strict, err)
	}
	_, err = e.srv.effectiveCeiling(withContextChoice(treeCtx("sub-cws-root", nil, ""), contextChoice{StrictPins: strict}))
	if ref := contextRunRefusal(err); ref == nil || ref.body.Reason != reasonWorkspaceUnitNotYours || !strings.Contains(ref.body.Error, "only runs in that unit") {
		t.Errorf("a Root member on a strict pin: %v, want the strict refusal", err)
	}
	if w := put(&u.eu.ID, nil); w.Code != http.StatusOK || !read().OwningUnitStrict || *read().OwningUnitID != u.eu.ID {
		t.Errorf("a move down with strict absent: %d %s, want ProdEU and still strict", w.Code, w.Body)
	}
	ch := e.pending(put(&u.eu.ID, &off))
	if !read().OwningUnitStrict {
		t.Fatal("a held strict -> plain changed the row")
	}
	e.approve(ch.ID)
	if read().OwningUnitStrict {
		t.Error("the approved strict -> plain left the pin strict")
	}
	if w := put(nil, &on); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonWorkspaceOwningUnitStrictNeedsUnit {
		t.Errorf("unit_id null with strict: %d %s, want 422 %s", w.Code, w.Body, reasonWorkspaceOwningUnitStrictNeedsUnit)
	}
}

// TestWorkspacePinPendingBeforeUpgradeStillApplies: a held move stored as an older build stored it (no
// strict key in the payload or the base state) applies against a row that has not moved.
func TestWorkspacePinPendingBeforeUpgradeStillApplies(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := seedCPTree(t, e, "cwu")
	ws := seedPinnableWorkspace(t, e, "cwu-ws")
	if _, err := e.pg.SetWorkspaceOwningUnit(context.Background(), ws.ID, &u.eu.ID); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"workspace_id": ws.ID, "unit_id": u.dev.ID})
	ch := types.GovernanceChange{ID: uuid.New(), Payload: payload, BaseHash: computeETag(map[string]any{"owning_unit_id": &u.eu.ID})}
	ctx := t.Context()
	tx, err := testutil.PGConn(t, e.pool).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed below; a no-op then
	r := httptest.NewRequest(http.MethodPost, "/x", nil).WithContext(ctx)
	if _, err := applyWorkspaceOwningUnitChange(e.srv, r, tx, ch); err != nil {
		t.Fatalf("an older build's held move: %v, want applied", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.pg.GetWorkspace(ctx, ws.ID)
	if err != nil || got.OwningUnitID == nil || *got.OwningUnitID != u.dev.ID || got.OwningUnitStrict {
		t.Errorf("after the apply: %v strict %v %v, want Dev, plain", got.OwningUnitID, got.OwningUnitStrict, err)
	}
}

// cpRun is a running run in unit carrying org component c.
func cpRun(t *testing.T, e *govEnv, owner string, unit uuid.UUID, c types.Component) types.AgentRun {
	t.Helper()
	ctx := t.Context()
	run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code", UnitID: &unit,
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pg.PutRunComponents(ctx, run.ID, []types.RunComponent{{ComponentID: &c.ID, Name: c.Name, Version: c.Version, Definition: c.Definition}}); err != nil {
		t.Fatal(err)
	}
	return run
}

// TestLiveMintRefusedWhenComponentPinMoves: a run in ProdEU carrying a component pinned to Prod mints;
// once the pin moves to Dev, a mint (and an injection redeem, the same door) is refused
// component_unit_pinned with a credential.mint denied row.
func TestLiveMintRefusedWhenComponentPinMoves(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := seedCPTree(t, e, "cpm")
	c := seedOrgComponent(t, e, "cpm-tool", "sub-cpm")
	pinComponent(t, e, c.ID, &u.prod.ID)
	run := cpRun(t, e, "sub-cpm", u.eu.ID, c)
	g, err := e.pg.CreateGrant(t.Context(), types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(pgOrgScope)}})
	if err != nil {
		t.Fatal(err)
	}
	claims := &identity.Claims{RunID: run.ID, SPIFFEID: "spiffe://wardyn.test/agent-run/" + run.ID.String()}
	door := func() (*httptest.ResponseRecorder, bool) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/internal/v1/mint", nil)
		_, done := e.srv.refuseWithheldGrant(w, r, claims, g.ID, func(_ liveGrant, err error) { t.Fatalf("unread: %v", err) })
		return w, done
	}
	if _, done := door(); done {
		t.Fatal("the door refused while the pin covers the run")
	}
	pinComponent(t, e, c.ID, &u.dev.ID)
	w, done := door()
	if !done || w.Code != http.StatusForbidden || wireReason(t, w) != reasonComponentUnitPinned {
		t.Fatalf("after the move: %v %d %s, want 403 %s", done, w.Code, w.Body, reasonComponentUnitPinned)
	}
	rows := e.audits("credential.mint")
	if len(rows) == 0 || rows[len(rows)-1].Outcome != "denied" || !strings.Contains(string(rows[len(rows)-1].Data), reasonComponentUnitPinned) {
		t.Errorf("credential.mint rows = %+v, want a denied row with %s", rows, reasonComponentUnitPinned)
	}
}

// TestReviveAndExtendRefusedWhenComponentPinMoves: the revive, restart and end extension re-check
// (ownerCapabilityRefusal) refuses a run whose component's pin no longer covers it, for any caller and
// for the owner's own admin session.
func TestReviveAndExtendRefusedWhenComponentPinMoves(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := seedCPTree(t, e, "cpv")
	c := seedOrgComponent(t, e, "cpv-tool", "sub-cpv")
	pinComponent(t, e, c.ID, &u.prod.ID)
	run := cpRun(t, e, "sub-cpv", u.eu.ID, c)
	owner := operatorCtx("sub-cpv", "cpv@corp.example", oidc.RoleUser)
	if ref, err := e.srv.ownerCapabilityRefusal(owner, run, true, nil); err != nil || (ref != nil && ref.reason == reasonComponentUnitPinned) {
		t.Fatalf("while the pin covers the run: %+v %v, want no pin refusal", ref, err)
	}
	pinComponent(t, e, c.ID, &u.dev.ID)
	for name, ctx := range map[string]context.Context{
		"the owner":                     owner,
		"the owner's own admin session": operatorCtx("sub-cpv", "cpv@corp.example", oidc.RoleAdmin),
	} {
		ref, err := e.srv.ownerCapabilityRefusal(ctx, run, true, nil)
		if err != nil || ref == nil || ref.status != http.StatusConflict || ref.reason != reasonComponentUnitPinned {
			t.Errorf("%s after the move: %+v %v, want 409 %s", name, ref, err, reasonComponentUnitPinned)
		}
	}
}

// TestDeleteUnitPinnedByComponent: deleting a unit a component is pinned to is a 409 naming the
// component, and a pin written after the guard's read is caught by the foreign key.
func TestDeleteUnitPinnedByComponent(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	unit := treeChild(t, e, `{"name":"cpx-unit","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	c := seedOrgComponent(t, e, "cpx-tool")
	pinComponent(t, e, c.ID, &unit.ID)
	w := e.admin(http.MethodDelete, "/api/v1/governance/units/"+unit.ID.String(), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceUnitInUse || !strings.Contains(w.Body.String(), "component(s) cpx-tool") {
		t.Fatalf("delete of a unit a component is pinned to: %d %s, want 409 naming the component", w.Code, w.Body)
	}
	// The foreign key: a raw delete past the guard is refused by components_owning_unit_id_fkey.
	_, err := e.pool.Exec(t.Context(), `DELETE FROM governance_units WHERE id = $1`, unit.ID)
	if err == nil || !strings.Contains(err.Error(), "components_owning_unit_id_fkey") {
		t.Errorf("a raw delete: %v, want the components foreign key", err)
	}
	var pinned *store.ErrUnitPinned
	if err := store.DeleteGovernanceUnitQ(t.Context(), e.pool, unit.ID); !errors.As(err, &pinned) || len(pinned.Components) != 1 {
		t.Errorf("the store's delete: %v, want ErrUnitPinned naming the component", err)
	}
	pinComponent(t, e, c.ID, nil)
	if w := e.admin(http.MethodDelete, "/api/v1/governance/units/"+unit.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Errorf("delete after the unpin: %d %s, want 204", w.Code, w.Body)
	}
}

// TestComponentPinOrgRowsOnly: a person's row cannot be pinned, through the store write or a direct
// UPDATE (the CHECK).
func TestComponentPinOrgRowsOnly(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	unit := treeChild(t, e, `{"name":"cpq-unit","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	mine, err := e.pg.CreateComponent(t.Context(), types.Component{ID: uuid.New(), Owner: "sub-cpq", Name: "cpq-mine"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetComponentOwningUnitQ(t.Context(), e.pool, mine.ID, &unit.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("pinning a person's row: %v, want ErrNotFound", err)
	}
	if _, err := store.ComponentOwningUnitQ(t.Context(), e.pool, mine.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading a person's row's pin: %v, want ErrNotFound", err)
	}
	_, err = e.pool.Exec(t.Context(), `UPDATE components SET owning_unit_id = $1 WHERE id = $2`, unit.ID, mine.ID)
	if err == nil || !strings.Contains(err.Error(), "components_owning_unit_org_only") {
		t.Errorf("a direct UPDATE of a person's row: %v, want the org-only CHECK", err)
	}
	ws := seedPinnableWorkspace(t, e, "cpq-ws")
	_, err = e.pool.Exec(t.Context(), `UPDATE workspaces SET owning_unit_strict = true WHERE id = $1`, ws.ID)
	if err == nil || !strings.Contains(err.Error(), "workspaces_owning_unit_strict_needs_unit") {
		t.Errorf("a strict row with no unit: %v, want the strict-needs-unit CHECK", err)
	}
}
