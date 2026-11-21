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

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedIssuerPortal binds a fresh issuer and registers an issuer portal on it (pinned when pin is set).
func seedIssuerPortal(t *testing.T, st store.PG, pin *uuid.UUID, shape ...func(*types.Delegate)) types.Delegate {
	t.Helper()
	ctx := context.Background()
	b, err := st.BindPortalIssuerQ(ctx, st.Pool, "https://idp-"+uuid.NewString()+".example.com", "https://internal-"+uuid.NewString()+".example.com", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	d := types.Delegate{ID: uuid.New(), Name: "portal-" + uuid.NewString(), IdPClientID: uuid.NewString(), RegisteredBy: "test",
		Issuer: b.Issuer, InternalIssuer: b.InternalIssuerNorm, IssuerKey: b.IssuerKey, RunInUnder: pin,
		TokenProfile: "okta_access_token", UserType: types.UserTypeStandard, MaxSubjectTokenAgeSeconds: 600, GrantTTLSeconds: 600,
		AdmissionRev: 1}
	for _, f := range shape {
		f(&d)
	}
	out, err := st.CreateDelegateQ(ctx, st.Pool, d, sha256HexOf(store.PortalCredentialPrefix+uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func withClaim(claim, value string) func(*types.Delegate) {
	return func(d *types.Delegate) {
		d.PortalManagedAdmission = true
		d.RequiredClaim = &types.PortalRequiredClaim{Claim: claim, Value: value}
	}
}

// admissionBody is a PUT /admin/delegates/{id}/admission body: the new rule and the one read.
func admissionBody(next, expected types.PortalAdmission) string {
	b, _ := json.Marshal(map[string]any{"group": next.Group, "portal_managed_admission": next.PortalManaged,
		"required_claim": next.RequiredClaim, "expected": expected})
	return string(b)
}

// An admission change is held under the second-human switch: the rule stays in force while it waits and after a
// rejection, a stale compare is refused, and an approval applies it once and moves the revision.
func TestPortalAdmission_ChangeHeldAndAudited_PG(t *testing.T) {
	e := newGovEnv(t)
	d := seedIssuerPortal(t, e.pg, nil, withClaim("tenant", "env-a"))
	cur := types.PortalAdmission{PortalManaged: true, RequiredClaim: d.RequiredClaim}
	next := types.PortalAdmission{PortalManaged: true} // the claim removed: a widening
	path := "/api/v1/admin/delegates/" + d.ID.String() + "/admission"
	second := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
	claimStays := func(when string) {
		t.Helper()
		got, err := e.pg.GetDelegateByID(context.Background(), d.ID)
		if err != nil || got.RequiredClaim == nil || got.AdmissionRev != 1 {
			t.Fatalf("%s: the portal row = claim %v rev %d (%v), want the claim in force at rev 1", when, got.RequiredClaim, got.AdmissionRev, err)
		}
	}

	ch := e.pending(e.call(e.super, http.MethodPut, path, admissionBody(next, cur)))
	claimStays("held")
	if w := e.call(second, http.MethodPost, rejectPath(ch.ID), `{}`); w.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", w.Code, w.Body)
	}
	claimStays("rejected")

	ch = e.pending(e.call(e.super, http.MethodPut, path, admissionBody(next, cur)))
	if w := e.call(second, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	got, _ := e.pg.GetDelegateByID(context.Background(), d.ID)
	if got.RequiredClaim != nil || got.AdmissionRev != 2 {
		t.Fatalf("after the approval: claim %v rev %d, want none at rev 2", got.RequiredClaim, got.AdmissionRev)
	}
	if rows := e.audits("delegate.admission.change"); len(rows) != 1 {
		t.Fatalf("delegate.admission.change rows = %d, want 1", len(rows))
	}
	// The rule read before the change is stale now.
	if w := e.call(e.super, http.MethodPut, path, admissionBody(next, cur)); w.Code != http.StatusConflict || wireReason(t, w) != reasonDelegateAdmissionStale {
		t.Fatalf("a stale compare = %d %s, want 409 %s", w.Code, w.Body, reasonDelegateAdmissionStale)
	}
	if w := e.call(e.super, http.MethodPut, path, admissionBody(types.PortalAdmission{}, types.PortalAdmission{PortalManaged: true})); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no group and no flag = %d %s, want 422", w.Code, w.Body)
	}
}

// The audit rows of a change carry the claim names and whether a group is set, never a value; the held change
// itself shows the value to the approver.
func TestPortalAdmission_AuditCarriesNamesNotValues_PG(t *testing.T) {
	e := newGovEnv(t)
	const was, now = "env-secret-one", "env-secret-two"
	d := seedIssuerPortal(t, e.pg, nil, withClaim("tenant", was))
	cur := types.PortalAdmission{PortalManaged: true, RequiredClaim: d.RequiredClaim}
	next := types.PortalAdmission{PortalManaged: true, RequiredClaim: &types.PortalRequiredClaim{Claim: "region", Value: now}}
	second := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
	w := e.call(e.super, http.MethodPut, "/api/v1/admin/delegates/"+d.ID.String()+"/admission", admissionBody(next, cur))
	ch := e.pending(w)
	if !strings.Contains(string(ch.Diff), now) {
		t.Errorf("the held change's diff = %s, want the new value shown to the approver", ch.Diff)
	}
	if w := e.call(second, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	rows := e.audits("delegate.admission.change")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"claim_before":"tenant"`) || !strings.Contains(string(rows[0].Data), `"claim_after":"region"`) {
		t.Fatalf("delegate.admission.change = %+v, want the claim names before and after", rows)
	}
	for _, ev := range e.h.audit.snapshot() {
		if strings.Contains(string(ev.Data), was) || strings.Contains(string(ev.Data), now) {
			t.Errorf("audit row %s carries a configured claim value: %s", ev.Action, ev.Data)
		}
	}
}

// A binding retires only while it admitted nobody: a mistyped issuer with a live portal is refused, the same
// binding retires once the portal is revoked, the right spelling registers after, and a binding with one identity
// stays.
func TestPortalIssuerBinding_RetireOnlyWhenUnused_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	retire := func(key string) *httptest.ResponseRecorder {
		return do(t, e.h.srv, http.MethodDelete, "/api/v1/admin/portal-issuers/"+key, adminToken, "")
	}
	typo := e.bind(t)
	d := e.portal(t, typo, nil)
	if r := retire(typo.IssuerKey); r.Code != http.StatusConflict || errorReason(r) != reasonPortalIssuerInUse {
		t.Fatalf("retire beside a live portal = %d %s, want 409 %s", r.Code, r.Body, reasonPortalIssuerInUse)
	}
	if _, _, err := e.st.RevokeDelegate(context.Background(), d.ID, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if r := retire(typo.IssuerKey); r.Code != http.StatusNoContent {
		t.Fatalf("retire after the revoke = %d %s, want 204", r.Code, r.Body)
	}
	if r := retire(typo.IssuerKey); r.Code != http.StatusNotFound {
		t.Fatalf("retire twice = %d, want 404", r.Code)
	}
	used := e.bind(t)
	d2 := e.portal(t, used, nil)
	e.person(t, d2, "alice")
	if _, _, err := e.st.RevokeDelegate(context.Background(), d2.ID, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if r := retire(used.IssuerKey); r.Code != http.StatusConflict {
		t.Fatalf("retire a binding with one identity = %d %s, want 409", r.Code, r.Body)
	}
}

// uiCallerBody is a launch body whose inline policy (or overlay) names caller on an inbound app.
func uiCallerBody(field, caller string) string {
	return `,"` + field + `":{"ui_apps":[{"name":"web","port":3000,"inbound":true,"callers":["user:` + caller + `"]}]}`
}

const unboundPerson = "delegate:person:aaaaaaaaaaaaaaaaaaaaaaaaaa:mfrggzdf"

// A launch, a preset or a template naming a portal person of an issuer with no binding is refused at write (422
// delegate_person_unknown_portal). A queued reference blocks the binding's retirement, so a waiting run cannot lose
// its binding in production; the start's own refusal is proved on an explicitly historical fixture, a binding
// removed under a waiting run by hand, and dispatches nothing.
func TestPortalSubjectRuling_UICallerUnboundRefusedAtCreateAndQueuedStart_PG(t *testing.T) {
	e := newQueueEnv(t)
	ctx := context.Background()
	pool := e.cappedPool("Busy", 1)
	refused := func(what string, code int, reason string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != code || errorReason(w) != reason {
			t.Errorf("%s naming an unbound portal person = %d %s, want %d %s", what, w.Code, w.Body, code, reason)
		}
	}
	for _, field := range []string{"inline_policy", "policy_overlay"} {
		_, w := e.launch(e.person, pool, uiCallerBody(field, unboundPerson))
		refused("a launch's "+field, http.StatusUnprocessableEntity, reasonDelegatePersonUnknownPortal, w)
	}
	apps := `{"ui_apps":[{"name":"web","port":3000,"inbound":true,"callers":["user:` + unboundPerson + `"]}]}`
	refused("a launch preset", http.StatusUnprocessableEntity, reasonDelegatePersonUnknownPortal,
		e.admin(http.MethodPut, "/api/v1/presets/unbound-callers", `{"request":{"agent":"claude-code","inline_policy":`+apps+`}}`))
	tmpl, _ := json.Marshal(map[string]any{"scope": "person", "name": "unbound-callers",
		"document": json.RawMessage(templateDocBody(`{"inline_policy":` + apps + `}`))})
	refused("a run template", http.StatusUnprocessableEntity, reasonDelegatePersonUnknownPortal,
		e.call(e.person, http.MethodPost, "/api/v1/templates", string(tmpl)))

	b, err := e.pg.BindPortalIssuerQ(ctx, e.pg.Pool, "https://idp-"+uuid.NewString()+".example.com", "https://internal-"+uuid.NewString()+".example.com", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(e.person, pool, uiCallerBody("inline_policy", types.PortalPrincipal(b.IssuerKey, "alice")))
	retire := func() error {
		return e.pg.WriteGovernance(ctx, func(q store.Querier) error {
			if err := store.LockPortalIssuerRegistry(ctx, q); err != nil {
				return err
			}
			return e.pg.RetirePortalIssuerQ(ctx, q, b.IssuerKey, nil)
		})
	}
	if err := retire(); !errors.Is(err, store.ErrIssuerInUse) {
		t.Fatalf("retire a binding a waiting run names = %v, want ErrIssuerInUse (the census reads run_queue.launch)", err)
	}
	// Historical fixture: the binding is gone under the waiting run, which production never allows.
	e.exec(`DELETE FROM portal_issuers WHERE issuer_key = $1`, b.IssuerKey)
	e.runner.mu.Lock()
	dispatched := e.runner.createCalls
	e.runner.mu.Unlock()
	e.free(occupant)
	got := e.waitFailed(waiting.ID)
	if !strings.Contains(got.StatusDetail, reasonDelegatePersonUnknownPortal) {
		t.Errorf("a queued run whose binding is gone = %s %q, want an end naming %s", got.State, got.StatusDetail, reasonDelegatePersonUnknownPortal)
	}
	e.runner.mu.Lock()
	after := e.runner.createCalls
	e.runner.mu.Unlock()
	if after != dispatched {
		t.Errorf("the refused start dispatched %d sandboxes, want none", after-dispatched)
	}
}

// A unit-admin grant held on 0.10.1 names a portal person; once the issuer is registered, an approval is
// refused and writes no grant, and the change stays pending so it can still be rejected.
func TestPortalSubjectRuling_OldHeldUnitAdminRefusedAtApproval_PG(t *testing.T) {
	e := newGovEnv(t)
	ctx := context.Background()
	d := seedIssuerPortal(t, e.pg, nil)
	unit := e.seedProfile("admins-unit", "api.anthropic.com")
	p := unitAdminPayload{UnitID: unit.ID, SubjectType: types.CapabilitySubjectUser, Subject: types.PortalPrincipal(d.IssuerKey, "alice")}
	var base any
	if err := e.pg.DryRunGovernance(ctx, func(q store.Querier) (err error) { base, _, err = unitAdminState(ctx, q, p, false); return }); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(p)
	ch, _, err := e.pg.ProposeGovernanceChange(ctx, types.GovernanceChange{
		TargetKind: govKindUnitAdmin, Op: "create", TargetKey: membershipKey(p.UnitID, p.SubjectType, p.Subject), Payload: payload,
		BaseHash: computeETag(base), DeploymentHash: computeETag(e.cfg.DefaultPolicy), ProposedBy: "sub-legacy-proposer",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.super, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve = %d %s, want 422 (a portal person is never a unit admin)", w.Code, w.Body)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM governance_unit_admins WHERE subject = $1`, p.Subject).Scan(&n); err != nil || n != 0 {
		t.Errorf("unit admin rows for the portal person = %d (%v), want none", n, err)
	}
	if got, err := e.pg.GetGovernanceChange(ctx, ch.ID); err != nil || got.State != types.GovernanceChangePending {
		t.Errorf("the change after the refused approval = %+v (%v), want pending", got.State, err)
	}
}

func bindForRetire(t *testing.T, st store.PG) types.PortalIssuer {
	t.Helper()
	b, err := st.BindPortalIssuerQ(context.Background(), st.Pool, "https://idp-"+uuid.NewString()+".example.com", "https://internal-"+uuid.NewString()+".example.com", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The configured default policy lives in the process, not the database, so the census cannot see it: a person
// of the binding named only there still keeps the binding, in the direct path, at the proposal and at the decision.
func TestPortalIssuerBinding_DefaultPolicyReferenceRefused_PG(t *testing.T) {
	named := func(key string) []types.UIApp {
		return []types.UIApp{{Name: "web", Port: 3000, Inbound: true, Callers: []string{"user:" + types.PortalPrincipal(key, "alice")}}}
	}
	// The direct path (no second-human switch).
	e := newAuthorityEnv(t)
	b := e.bind(t)
	e.h.srv.cfg.DefaultPolicy.UIApps = named(b.IssuerKey)
	if w := do(t, e.h.srv, http.MethodDelete, "/api/v1/admin/portal-issuers/"+b.IssuerKey, adminToken, ""); w.Code != http.StatusConflict || errorReason(w) != reasonPortalIssuerInUse {
		t.Fatalf("direct retire beside a default policy reference = %d %s, want 409 %s", w.Code, w.Body, reasonPortalIssuerInUse)
	}
	e.h.srv.cfg.DefaultPolicy.UIApps = nil
	if w := do(t, e.h.srv, http.MethodDelete, "/api/v1/admin/portal-issuers/"+b.IssuerKey, adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("direct retire once the reference is gone = %d %s, want 204", w.Code, w.Body)
	}

	// The held path: refused at the proposal, and at the decision when the reference arrived after it.
	g := newGovEnv(t)
	held := bindForRetire(t, g.pg)
	path := "/api/v1/admin/portal-issuers/" + held.IssuerKey
	second := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
	g.srv.cfg.DefaultPolicy.UIApps = named(held.IssuerKey)
	if w := g.call(g.super, http.MethodDelete, path, ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonPortalIssuerInUse {
		t.Fatalf("held retire beside a default policy reference = %d %s, want 409 %s at the proposal", w.Code, w.Body, reasonPortalIssuerInUse)
	}
	g.srv.cfg.DefaultPolicy.UIApps = nil
	ch := g.pending(g.call(g.super, http.MethodDelete, path, ""))
	g.srv.cfg.DefaultPolicy.UIApps = named(held.IssuerKey)
	if w := g.call(second, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusConflict || wireReason(t, w) != reasonPortalIssuerInUse {
		t.Fatalf("approve beside a default policy reference = %d %s, want 409 %s", w.Code, w.Body, reasonPortalIssuerInUse)
	}
	if bound, err := store.IssuerKeyBoundQ(context.Background(), g.pg.Pool, held.IssuerKey); err != nil || !bound {
		t.Errorf("the binding after the refused approval = %v (%v), want still bound", bound, err)
	}
}

// A held retirement is for the binding that was reviewed: the key retired and bound again meanwhile is a
// different binding, and the approval is stale and leaves it in place.
func TestPortalIssuerBinding_HeldRetireRefusesReplacementBinding_PG(t *testing.T) {
	g := newGovEnv(t)
	ctx := context.Background()
	b := bindForRetire(t, g.pg)
	second := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
	ch := g.pending(g.call(g.super, http.MethodDelete, "/api/v1/admin/portal-issuers/"+b.IssuerKey, ""))
	err := g.pg.WriteGovernance(ctx, func(q store.Querier) error {
		if err := store.LockPortalIssuerRegistry(ctx, q); err != nil {
			return err
		}
		return g.pg.RetirePortalIssuerQ(ctx, q, b.IssuerKey, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := g.pg.BindPortalIssuerQ(ctx, g.pg.Pool, b.Issuer, b.InternalIssuerNorm, b.PersonIDClaim, "test")
	if err != nil || again.IssuerKey != b.IssuerKey || !again.CreatedAt.After(b.CreatedAt) {
		t.Fatalf("rebind = %+v (%v), want the same key with a later creation time", again, err)
	}
	if w := g.call(second, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approve against a replacement binding = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if bound, err := store.IssuerKeyBoundQ(ctx, g.pg.Pool, b.IssuerKey); err != nil || !bound {
		t.Errorf("the replacement binding after the refused approval = %v (%v), want still bound", bound, err)
	}
}

// A held admission change for a portal revoked while it waited is stale: nothing is written to the revoked row.
func TestPortalAdmission_HeldChangeAfterRevokeIsStale_PG(t *testing.T) {
	g := newGovEnv(t)
	ctx := context.Background()
	d := seedIssuerPortal(t, g.pg, nil, withClaim("tenant", "env-a"))
	cur := types.PortalAdmission{PortalManaged: true, RequiredClaim: d.RequiredClaim}
	second := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
	ch := g.pending(g.call(g.super, http.MethodPut, "/api/v1/admin/delegates/"+d.ID.String()+"/admission", admissionBody(types.PortalAdmission{PortalManaged: true}, cur)))
	if _, _, err := g.pg.RevokeDelegate(ctx, d.ID, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if w := g.call(second, http.MethodPost, approvePath(ch.ID), `{}`); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approve after the portal was revoked = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	got, err := g.pg.GetDelegateByID(ctx, d.ID)
	if err != nil || got.RequiredClaim == nil || got.AdmissionRev != d.AdmissionRev || got.RevokedAt == nil {
		t.Errorf("the revoked row = claim %v rev %d revoked %v (%v), want it untouched", got.RequiredClaim, got.AdmissionRev, got.RevokedAt, err)
	}
}
