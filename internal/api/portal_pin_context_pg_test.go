// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An issuer portal's person with no membership runs in the portal's pin and the units under it, on a real
// Postgres. Strict workspaces and pinned organisation components still need a membership the person holds, at
// launch and at every later re-check of the owner.

// pinCtxBody is pinRun with extra fields (each with a leading comma) before run_in.
func pinCtxBody(runIn, extra string) string {
	b := pinRunBody + extra
	if runIn != "" {
		b += `,"run_in":"` + runIn + `"`
	}
	return b + `}`
}

// createdRunOf requires a 201 and reads the run it created.
func createdRunOf(t *testing.T, st store.PG, w *httptest.ResponseRecorder) types.AgentRun {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("launch = %d %s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	run, err := st.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("read the created run: %v", err)
	}
	return run
}

func runUnitOf(run types.AgentRun) uuid.UUID {
	if run.UnitID == nil {
		return uuid.Nil
	}
	return *run.UnitID
}

// preflightAgrees posts body to preflight and then to create through tok, requires the two to agree (200 with
// 201, else the same status and reason) and returns create's answer.
func preflightAgrees(t *testing.T, srv *Server, tok, body string) *httptest.ResponseRecorder {
	t.Helper()
	pre := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", tok, body)
	cre := do(t, srv, http.MethodPost, "/api/v1/runs", tok, body)
	switch {
	case cre.Code == http.StatusCreated:
		if pre.Code != http.StatusOK {
			t.Errorf("%s: create 201, preflight %d %s; want 200", body, pre.Code, pre.Body.String())
		}
	case pre.Code != cre.Code || errorReason(pre) != errorReason(cre):
		t.Errorf("%s: preflight %d %q, create %d %q; want the same answer", body, pre.Code, errorReason(pre), cre.Code, errorReason(cre))
	}
	return cre
}

func wantPinReason(t *testing.T, name string, w *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if w.Code != status || errorReason(w) != reason {
		t.Errorf("%s = %d %s, want %d %s", name, w.Code, w.Body.String(), status, reason)
	}
}

// personRow gives principal an actual user membership of u.
func (e *authorityEnv) personRow(t *testing.T, principal string, u types.GovernanceUnit) types.GovernanceMembership {
	t.Helper()
	m, err := e.st.UpsertGovernanceMembership(context.Background(), types.GovernanceMembership{ID: uuid.New(),
		SubjectType: types.CapabilitySubjectUser, Subject: types.CanonicalUserSubject(principal), UnitID: u.ID, Priority: 1})
	if err != nil {
		t.Fatalf("seed %s's membership of %s: %v", principal, u.Name, err)
	}
	return m
}

func (e *authorityEnv) dropRow(t *testing.T, m types.GovernanceMembership) {
	t.Helper()
	if _, err := e.st.DeleteGovernanceMembership(context.Background(), m.ID); err != nil {
		t.Fatalf("remove the membership: %v", err)
	}
}

// ownerChecks is each re-check of the owner a live run meets: an extension, a resume and a revive. Each is
// asked as someone other than the owner, so the run row is all that is known of them.
func (e *authorityEnv) ownerChecks(run types.AgentRun) map[string]string {
	ctx := context.Background()
	out := map[string]string{}
	if ref := e.h.srv.extendRefusal(ctx, run, false); ref != nil {
		out["extend"] = ref.reason
	} else {
		out["extend"] = ""
	}
	if ref := e.h.srv.resumeAuthorityRefusal(ctx, run); ref != nil {
		out["resume"] = ref.reason
	} else {
		out["resume"] = ""
	}
	if _, rerr := e.h.srv.reviveCeiling(ctx, run, types.ActorHuman, "sub-operator"); rerr != nil {
		out["revive"] = rerr.reason
	} else {
		out["revive"] = ""
	}
	return out
}

func wantOwnerChecks(t *testing.T, name string, got map[string]string, reason string) {
	t.Helper()
	for check, r := range got {
		if r != reason {
			t.Errorf("%s, %s = %q, want %q", name, check, r, reason)
		}
	}
}

// The baseline: a fresh portal person with no membership launches in the pin by default, in the pin or the unit
// under it when named, and is refused a unit outside it or a name no unit holds. Preflight agrees with each.
func TestPortalPinContext_FreshPortalNoMembership_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	d := e.portal(t, e.bind(t), &e.plat.ID)
	principal, tok := e.person(t, d, "fresh")
	ms, err := e.st.ListMembershipsFor(context.Background(), []string{principal}, nil, d.UserType)
	if err != nil || len(ms) != 0 {
		t.Fatalf("the person's memberships = %+v %v, want none", ms, err)
	}
	for _, c := range []struct {
		runIn string
		want  uuid.UUID
	}{{"", e.plat.ID}, {e.team.Name, e.team.ID}, {e.plat.Name, e.plat.ID}} {
		if got := runUnitOf(createdRunOf(t, e.st, preflightAgrees(t, e.h.srv, tok, pinRun(c.runIn)))); got != c.want {
			t.Errorf("run_in %q landed in %s, want %s", c.runIn, got, c.want)
		}
	}
	for _, runIn := range []string{e.other.Name, "no-such-unit"} {
		wantPinReason(t, "run_in "+runIn, preflightAgrees(t, e.h.srv, tok, pinRun(runIn)), http.StatusForbidden, "unit_not_a_context")
	}
}

// The real sequence: the portal is registered through the admin route as the super admin, the person exchanges a
// token at the real token endpoint, and the grant then launches in the pin with no membership.
func TestPortalPinContext_RealRegistrationAndExchange_PG(t *testing.T) {
	e := newExchPG(t)
	ctx := context.Background()
	if _, err := e.st.ActivatePortalIssuers(ctx, "test", nil); err != nil {
		t.Fatalf("switch issuer portals on: %v", err)
	}
	unit := func(u types.GovernanceUnit) types.GovernanceUnit {
		u.ID, u.CreatedBy = uuid.New(), "seed"
		got, err := e.st.UpsertGovernanceUnit(ctx, u)
		if err != nil {
			t.Fatalf("seed unit %q: %v", u.Name, err)
		}
		return got
	}
	plat := unit(types.GovernanceUnit{Name: "reg-platform", Ceiling: types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}})
	team := unit(types.GovernanceUnit{Name: "reg-team", ParentID: &plat.ID, Overlay: &types.CeilingOverlay{}})
	body := (&regPG{delegationPG: e.delegationPG}).body(map[string]any{"issuer": portalTestIssuer, "run_in_under": plat.ID.String()})
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/delegates", e.admin, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("register the issuer portal = %d %s, want 201", w.Code, w.Body.String())
	}
	var reg struct {
		ID         uuid.UUID `json:"id"`
		Credential string    `json:"credential"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil || reg.Credential == "" {
		t.Fatalf("the registration answer %s: %v, want an id and a credential", w.Body.String(), err)
	}
	tok, _ := portalToken(t, e.exchange(t, reg.ID, reg.Credential, "subject-token"))
	if ms, err := e.st.ListMembershipsFor(ctx, []string{e.ver.subj.Principal}, nil, types.UserTypeStandard); err != nil || len(ms) != 0 {
		t.Fatalf("the person's memberships = %+v %v, want none", ms, err)
	}
	if pre := do(t, e.h.srv, http.MethodPost, "/api/v1/runs/preflight", tok, pinRun("")); pre.Code != http.StatusOK {
		t.Errorf("preflight with no run_in = %d %s, want 200 in the pin", pre.Code, pre.Body.String())
	}
	if got := runUnitOf(createdRunOf(t, e.st, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok, pinRun(team.Name)))); got != team.ID {
		t.Errorf("run_in team landed in %s, want %s", got, team.ID)
	}
	if got := runUnitOf(createdRunOf(t, e.st, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok, pinRun("")))); got != plat.ID {
		t.Errorf("no run_in landed in %s, want the pin %s", got, plat.ID)
	}
}

// strictFixture is a portal person on the platform pin, granted a strict workspace and an organisation component
// both pinned to the team.
type strictFixture struct {
	principal, tok string
	ws, comp       uuid.UUID
}

func (e *authorityEnv) strictFixture(t *testing.T) strictFixture {
	t.Helper()
	ctx := context.Background()
	d := e.portal(t, e.bind(t), &e.plat.ID)
	principal, tok := e.person(t, d, "strict-"+uuid.NewString())
	ws, err := e.st.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: "strict-" + uuid.NewString()[:8], Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("seed the workspace: %v", err)
	}
	if err := store.SetWorkspaceOwningUnitQ(ctx, e.st.Pool, ws.ID, &e.team.ID, true); err != nil {
		t.Fatalf("pin the workspace strictly: %v", err)
	}
	if _, err := e.st.UpsertCapabilityGrant(ctx, grant(types.CapabilitySubjectUser, principal, capWorkspace, ws.ID.String(), types.CapabilityAllow)); err != nil {
		t.Fatal(err)
	}
	c, err := e.st.CreateComponent(ctx, types.Component{ID: uuid.New(), Name: "pinned-" + uuid.NewString()[:8], CreatedBy: "admin",
		Definition: types.ComponentDefinition{Hosts: []string{"api.anthropic.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetCapabilityRestriction(ctx, capComponent, c.ID.String(), true, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, g := range []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, d.UserType, capComponent, c.ID.String(), types.CapabilityAllow),
		grant(types.CapabilitySubjectUser, principal, capComponent, c.ID.String(), types.CapabilityAllow)} {
		if _, err := e.st.UpsertCapabilityGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetComponentOwningUnitQ(ctx, e.st.Pool, c.ID, &e.team.ID); err != nil {
		t.Fatal(err)
	}
	return strictFixture{principal: principal, tok: tok, ws: ws.ID, comp: c.ID}
}

func (f strictFixture) wsBody() string {
	return pinCtxBody("", `,"workspace_id":"`+f.ws.String()+`"`)
}

func (f strictFixture) compBody() string {
	return pinCtxBody("", `,"components":[{"id":"`+f.comp.String()+`"}]`)
}

// A strict workspace and a pinned organisation component need the person's own membership in the pinned unit,
// at create and preflight alike: the portal's pin never satisfies them. An ordinary run in the same unit needs
// none.
func TestPortalPinContext_StrictResourcesNeedActualMembership_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	f := e.strictFixture(t)
	wantPinReason(t, "a strict workspace with no membership", preflightAgrees(t, e.h.srv, f.tok, f.wsBody()), http.StatusForbidden, reasonWorkspaceUnitNotYours)
	wantPinReason(t, "a pinned component with no membership", preflightAgrees(t, e.h.srv, f.tok, f.compBody()), http.StatusForbidden, reasonComponentUnitPinned)
	if got := runUnitOf(createdRunOf(t, e.st, preflightAgrees(t, e.h.srv, f.tok, pinRun(e.team.Name)))); got != e.team.ID {
		t.Errorf("the ordinary control landed in %s, want the team %s", got, e.team.ID)
	}
	e.personRow(t, f.principal, e.team)
	for name, body := range map[string]string{"strict workspace": f.wsBody(), "pinned component": f.compBody()} {
		if got := runUnitOf(createdRunOf(t, e.st, preflightAgrees(t, e.h.srv, f.tok, body))); got != e.team.ID {
			t.Errorf("a %s with the team membership landed in %s, want the team %s", name, got, e.team.ID)
		}
	}
}

// After launch, the same split holds at every re-check of the owner (extend, resume, revive): a run with a strict
// resource needs the person's own membership, so withdrawing it refuses the run; an ordinary run stays held by
// the live portal's pin.
func TestPortalPinContext_LifecycleStrictNeedsMembership_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	f := e.strictFixture(t)
	m := e.personRow(t, f.principal, e.team)
	runs := map[string]types.AgentRun{
		"ordinary":         createdRunOf(t, e.st, e.post(t, "/api/v1/runs", f.tok, pinRun(e.team.Name))),
		"strict workspace": createdRunOf(t, e.st, e.post(t, "/api/v1/runs", f.tok, f.wsBody())),
		"pinned component": createdRunOf(t, e.st, e.post(t, "/api/v1/runs", f.tok, f.compBody())),
	}
	for name, run := range runs {
		if runUnitOf(run) != e.team.ID {
			t.Fatalf("the %s run landed in %s, want the team", name, runUnitOf(run))
		}
		wantOwnerChecks(t, name+" run with the membership", e.ownerChecks(run), "")
	}
	e.dropRow(t, m)
	wantOwnerChecks(t, "ordinary run after the withdrawal", e.ownerChecks(runs["ordinary"]), "")
	for _, name := range []string{"strict workspace", "pinned component"} {
		wantOwnerChecks(t, name+" run after the withdrawal", e.ownerChecks(runs[name]), reasonUnitMembershipGone)
	}
}

// A membership outside the pin is never the default pick, and naming it is the pin's refusal.
func TestPortalPinContext_OutsideMembershipNotPicked_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	ctx := context.Background()
	const typ = "pinctx-outside"
	if _, err := e.st.CreateUserType(ctx, types.UserType{ID: typ, Name: typ, Priority: 1, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertGovernanceMembership(ctx, types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUserType,
		Subject: typ, UnitID: e.other.ID, Priority: 5, Home: true}); err != nil {
		t.Fatal(err)
	}
	d := e.portal(t, e.bind(t), &e.plat.ID, func(d *types.Delegate) { d.UserType = typ })
	_, tok := e.person(t, d, "outside-row")
	if got := e.launchUnit(t, tok, ""); got != e.plat.ID {
		t.Errorf("no run_in with an outside home row landed in %s, want the pin %s", got, e.plat.ID)
	}
	if w := preflightAgrees(t, e.h.srv, tok, pinRun(e.other.Name)); !portalScopeRefused(w) {
		t.Errorf("run_in naming the outside unit = %d %s, want 403 portal_unit_scope", w.Code, w.Body.String())
	}
}

// Two portals on one issuer share its people, and each pin is its own portal's: the same person reaches each
// pin through its own portal only.
func TestPortalPinContext_AnotherPortalsPinIsItsOwn_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	b := e.bind(t)
	a := e.portal(t, b, &e.plat.ID)
	other := e.portal(t, b, &e.other.ID)
	_, tokA := e.person(t, a, "shared")
	_, tokB := e.person(t, other, "shared")
	if got := e.launchUnit(t, tokB, ""); got != e.other.ID {
		t.Errorf("through portal B with no run_in landed in %s, want %s", got, e.other.ID)
	}
	wantPinReason(t, "through portal B, run_in team", preflightAgrees(t, e.h.srv, tokB, pinRun(e.team.Name)), http.StatusForbidden, "unit_not_a_context")
	wantPinReason(t, "through portal A, run_in other", preflightAgrees(t, e.h.srv, tokA, pinRun(e.other.Name)), http.StatusForbidden, "unit_not_a_context")
}

// A console sign-in takes its contexts from memberships alone, as at 0.10.2.
func TestPortalPinContext_ConsoleIdentityUnchanged_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	session := accessSession(t, "sub-pinctx-console", "pinctx-console@corp.example", oidc.RoleUser, []string{})
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", session, pinRun(e.plat.Name)); w.Code != http.StatusForbidden || errorReason(w) != "unit_not_a_context" {
		t.Errorf("a console person naming the platform = %d %s, want 403 unit_not_a_context", w.Code, w.Body.String())
	}
	if run := createdRunOf(t, e.st, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", session, pinRun(""))); run.UnitID != nil {
		t.Errorf("a console person with no membership landed in %s, want the Default unit (no unit)", *run.UnitID)
	}
}

// A suspended person and a revoked portal's person are refused at authentication, before any placement, and
// leave no run.create row.
func TestPortalPinContext_SuspendedOrRevokedRefused_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	e.h.srv.cfg.SessionRevocations = &pgTestRevocations{pool: e.pool, st: e.st}
	e.h.srv.cfg.Audit = &tableTee{inner: e.h.srv.cfg.Audit, table: store.Recorder{Pool: e.pool}, seen: map[uuid.UUID]bool{}}
	d := e.portal(t, e.bind(t), &e.plat.ID)
	suspended, tok := e.person(t, d, "suspended")
	revoked := e.portal(t, e.bind(t), &e.plat.ID)
	_, revokedTok := e.person(t, revoked, "revoked-portal")
	before := len(e.rows("run.create"))
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/people/"+url.PathEscape(suspended)+"/suspend", e.secAdmin, ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s, want 200", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/admin/delegates/"+revoked.ID.String(), e.secAdmin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke the portal = %d %s, want 204", w.Code, w.Body.String())
	}
	for name, bearer := range map[string]string{"suspended person": tok, "revoked portal's person": revokedTok} {
		if w := e.post(t, "/api/v1/runs", bearer, pinRun("")); w.Code != http.StatusUnauthorized {
			t.Errorf("%s's launch = %d %s, want 401", name, w.Code, w.Body.String())
		}
	}
	if after := len(e.rows("run.create")); after != before {
		t.Errorf("run.create rows went from %d to %d, want no new row", before, after)
	}
}

// The pin context's walls are the unit's: preflight resolves the same policy for a person placed by the pin as for
// a console person holding a real membership there, and a restriction covering the portal's user type still clamps.
func TestPortalPinContext_WallsAreTheUnits_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	ctx := context.Background()
	_, pinTok := e.person(t, e.portal(t, e.bind(t), &e.plat.ID), "walls-pin")
	const memberSub = "sub-pinctx-walls-member"
	if _, err := e.st.UpsertGovernanceMembership(ctx, types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser,
		Subject: memberSub, UnitID: e.plat.ID, Priority: 1}); err != nil {
		t.Fatal(err)
	}
	member := accessSession(t, memberSub, "pinctx-walls@corp.example", oidc.RoleUser, []string{})
	fields := func(w *httptest.ResponseRecorder) map[string]string {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("preflight = %d %s, want 200", w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, k := range []string{"enforced_confinement_class", "limits", "provenance", "allowed_models"} {
			out[k] = string(body[k])
		}
		return out
	}
	got := fields(e.post(t, "/api/v1/runs/preflight", pinTok, pinRun(e.plat.Name)))
	want := fields(doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs/preflight", member, pinRun(e.plat.Name)))
	for k := range want {
		if got[k] != want[k] {
			t.Errorf("preflight %s: pin context %s, member %s; want the same", k, got[k], want[k])
		}
	}
	const typ = "pinctx-walls"
	if _, err := e.st.CreateUserType(ctx, types.UserType{ID: typ, Name: typ, Priority: 1, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	_, typedTok := e.person(t, e.portal(t, e.bind(t), &e.plat.ID, func(d *types.Delegate) { d.UserType = typ }), "walls-typed")
	if _, err := e.st.PutRestriction(ctx, types.Restriction{ID: uuid.New(), Name: "pinctx-no-shell",
		Limits:  types.RestrictionLimits{DenyInteractive: new(true)},
		Contact: &policyref.Contact{Owner: "pinctx-owner", Email: "pinctx@corp.example"},
		Members: []types.RestrictionMember{{SubjectType: types.CapabilitySubjectUserType, Subject: typ}}}, "seed"); err != nil {
		t.Fatal(err)
	}
	interactive := pinCtxBody(e.team.Name, `,"interactive":true`)
	if w := preflightAgrees(t, e.h.srv, typedTok, interactive); w.Code == http.StatusCreated || portalScopeRefused(w) {
		t.Errorf("an interactive launch under a restriction on the type = %d %s, want the restriction's refusal", w.Code, w.Body.String())
	}
	if w := e.post(t, "/api/v1/runs", pinTok, interactive); w.Code != http.StatusCreated {
		t.Errorf("the same launch by a person the restriction does not cover = %d %s, want 201", w.Code, w.Body.String())
	}
}

// A run placed by the pin passes the owner re-check while its portal is registered, and no longer once the portal
// is revoked. A run in the Default unit, through a portal pinned there, needs no membership at all.
func TestPortalPinContext_ExtendAfterRevoke_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	d := e.portal(t, e.bind(t), &e.plat.ID)
	_, tok := e.person(t, d, "extend")
	run := createdRunOf(t, e.st, e.post(t, "/api/v1/runs", tok, pinRun(e.team.Name)))
	if runUnitOf(run) != e.team.ID {
		t.Fatalf("the run landed in %s, want the team", runUnitOf(run))
	}
	wantOwnerChecks(t, "a pin-placed run, portal live", e.ownerChecks(run), "")
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/admin/delegates/"+d.ID.String(), e.secAdmin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke the portal = %d %s, want 204", w.Code, w.Body.String())
	}
	if _, ref := e.h.srv.ownerUnit(context.Background(), run); ref == nil || ref.reason != reasonUnitMembershipGone {
		t.Errorf("the owner check after the revoke = %+v, want %s", ref, reasonUnitMembershipGone)
	}
	def := e.portal(t, e.bind(t), &store.DefaultUnitID)
	person, _ := e.person(t, def, "default")
	inDefault := e.run(t, person, nil, def.ID)
	if _, ref := e.h.srv.ownerUnit(context.Background(), inDefault); ref != nil {
		t.Errorf("a run in the Default unit with no membership = %+v, want it to pass", ref)
	}
}

// An elevation never widens the pin: outside it, a named elevation is the pin's refusal, and with no run_in it is
// never picked. Inside it, a run placed while the elevation is live keeps passing the owner re-check after the
// elevation ends, since the pin, not the elevation, holds it.
func TestPortalPinContext_ElevationInAndOutOfPin_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	ctx := context.Background()
	d := e.portal(t, e.bind(t), &e.plat.ID)
	principal, tok := e.person(t, d, "elevated")
	elevate := func(u types.GovernanceUnit) types.GovernanceMembership {
		m := e.personRow(t, principal, u)
		if _, err := e.st.Pool.Exec(ctx, `UPDATE governance_memberships SET expires_at = now() + interval '1 hour' WHERE id = $1`, m.ID); err != nil {
			t.Fatalf("make the membership an elevation: %v", err)
		}
		return m
	}
	inPin, outside := elevate(e.team), elevate(e.other)
	if w := preflightAgrees(t, e.h.srv, tok, pinRun(e.other.Name)); !portalScopeRefused(w) {
		t.Errorf("run_in naming the outside elevation = %d %s, want 403 portal_unit_scope", w.Code, w.Body.String())
	}
	if got := e.launchUnit(t, tok, ""); got != e.plat.ID {
		t.Errorf("no run_in with two elevations landed in %s, want the pin %s", got, e.plat.ID)
	}
	run := createdRunOf(t, e.st, e.post(t, "/api/v1/runs", tok, pinRun(e.team.Name)))
	if runUnitOf(run) != e.team.ID {
		t.Fatalf("run_in naming the in-pin elevation landed in %s, want the team", runUnitOf(run))
	}
	for _, m := range []types.GovernanceMembership{inPin, outside} {
		if _, err := e.st.Pool.Exec(ctx, `UPDATE governance_memberships SET expires_at = now() - interval '1 second' WHERE id = $1`, m.ID); err != nil {
			t.Fatalf("end the elevation: %v", err)
		}
	}
	wantOwnerChecks(t, "a pin-placed run after the elevation ended", e.ownerChecks(run), "")
}
