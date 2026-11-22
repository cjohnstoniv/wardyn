// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// freshExchange is an exchange whose subject token was issued after anything the test did so far, as a person who
// signs in at the provider again would have.
func (l *portalLife) freshExchange(t *testing.T, uid string) int {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	l.ver.subj = l.person(uid, "")
	l.ver.subj.IssuedAt = time.Now()
	return l.exchange(t, l.d.ID, l.cred, "subject-token").Code
}

// suspendAndPurge suspends the person and purges them permanently, as the super admin.
func (l *portalLife) suspendAndPurge(t *testing.T, principal string) {
	t.Helper()
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", w.Code, w.Body.String())
	}
	if w := l.post(t, l.admin, principal, "purge", ""); w.Code != http.StatusOK {
		t.Fatalf("purge = %d %s", w.Code, w.Body.String())
	}
}

// After a permanent purge the next exchange, through any portal, is refused as deactivated, nothing is
// created, the display email is blank, and the tombstone cannot be undone.
func TestPortalPurge_TombstoneRefusesExchangeAndBlanksEmail_PG(t *testing.T) {
	l := newPortalLife(t)
	idPortal, idCred := l.portal(t, portalTestIssuer, func(d *types.Delegate) {
		d.TokenProfile, d.Audience, d.RequiredScopes, d.EmailDomains = oidc.PortalProfileIDToken, "", nil, []string{"corp.example"}
	})
	l.ver.subj = l.person("u1", "")
	l.ver.subj.Email, l.ver.subj.EmailVerified = "pat@corp.example", true
	principal := l.ver.subj.Principal
	l.token(t, idPortal, idCred)
	displayEmail := func() string {
		var e string
		if err := l.pool.QueryRow(context.Background(), `SELECT display_email FROM principal_identities WHERE principal = $1`, principal).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	if displayEmail() == "" {
		t.Fatal("control: the exchange stored no display email")
	}
	// A security admin may not purge.
	if w := l.post(t, l.secAdmin, principal, "purge", ""); w.Code != http.StatusForbidden {
		t.Fatalf("security admin purge = %d, want 403", w.Code)
	}
	if w := l.post(t, l.admin, principal, "purge", ""); w.Code != http.StatusConflict || errorReason(w) != "person_not_suspended" {
		t.Fatalf("purge of an active person = %d %s, want 409 person_not_suspended", w.Code, w.Body.String())
	}
	l.suspendAndPurge(t, principal)
	ident := l.ident(t, principal)
	if ident.PurgedAt == nil || displayEmail() != "" {
		t.Fatalf("identity after the purge: purged %v, display email %q; want a tombstone with no email", ident.PurgedAt, displayEmail())
	}
	if !l.allDone(t, ident.ID) {
		t.Errorf("ledger not done after the purge:\n%s", ledgerText(t, l.pool, ident.ID))
	}
	if got := l.freshExchange(t, "u1"); got != http.StatusBadRequest {
		t.Errorf("exchange after the purge = %d, want 400", got)
	}
	l.ver.subj = l.person("u1", "")
	l.ver.subj.IssuedAt = time.Now()
	if got := l.exchange(t, l.d.ID, l.cred, "subject-token").Code; got != http.StatusBadRequest {
		t.Errorf("exchange through another portal after the purge = %d, want 400", got)
	}
	if n := l.count(t, `SELECT count(*) FROM principal_identities WHERE principal = $1`, principal); n != 1 {
		t.Errorf("identity rows for the purged person = %d, want the one tombstone", n)
	}
	if n := l.count(t, `SELECT count(*) FROM portal_grants WHERE principal = $1`, principal); n != 0 {
		t.Errorf("portal_grants rows after the purge = %d, want 0", n)
	}
	for _, r := range l.deniedReasons() {
		if r != string(oidc.DenialIdentityDeactivated) {
			t.Errorf("denied reason = %s, want %s", r, oidc.DenialIdentityDeactivated)
		}
	}
	for _, action := range []string{"suspend", "reactivate"} {
		if w := l.post(t, l.admin, principal, action, ""); w.Code != http.StatusConflict || errorReason(w) != "person_purged" {
			t.Errorf("%s of a purged person = %d %s, want 409 person_purged", action, w.Code, w.Body.String())
		}
	}
	// The lookup shows the tombstone and no email.
	w := l.get(t, l.secAdmin, "/api/v1/people/portal-person?issuer="+url.QueryEscape(portalTestIssuer)+"&person_id=u1")
	var v portalPersonView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.State != "purged" || v.DisplayEmail != "" {
		t.Errorf("lookup after the purge = %d %s, want a purged person with no email", w.Code, w.Body.String())
	}
}

// A portal purge hands the person's workspaces to the operator whatever the SCIM setting says: the SCIM keep
// policy is the workforce's and is not read for them, and the workforce still reads it.
func TestPortalPurge_WorkspacePolicyUnchanged_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	l.scimOn(lifeWorkforceIssuer, time.Hour)
	l.h.srv.cfg.SCIM.KeepWorkspaces = true
	principal, _ := l.signIn(t, "u1")
	ws, err := l.st.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "ws-u1", OwnedBy: principal, Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	l.suspendAndPurge(t, principal)
	got, err := l.st.GetWorkspace(ctx, ws.ID)
	if err != nil || got.OwnedBy != "" {
		t.Errorf("workspace owner after a portal purge = %q, %v; want the operator", got.OwnedBy, err)
	}
	rows := l.rows("workspace.reassign")
	if len(rows) != 1 || lifeData(rows[0])["reason"] != "person_purge" || lifeData(rows[0])["from_owner"] != principal {
		t.Errorf("workspace.reassign rows = %+v, want one from the portal person", rows)
	}
	if !l.h.srv.scimOrigin().keepWorkspaces || portalOrigin(types.ActorSystem, "x").keepWorkspaces {
		t.Error("the SCIM origin must keep workspaces under the keep setting and the portal origin must not")
	}
}

// A reactivation that lands between the sweep's selection and its mark wins: no tombstone, no purge job.
func TestPortalPurge_ReactivatedAfterSelectionNotPurged_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", `{"purge_after_seconds": 1}`); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d", w.Code)
	}
	id := l.ident(t, principal).ID
	l.waitDue(t, id)
	due, err := l.st.PurgeDuePortalIdentities(ctx, time.Now().UTC(), scimSweepBatch)
	if err != nil || len(due) != 1 || due[0].ID != id || !due[0].Suspended {
		t.Fatalf("control: the selection = %+v, %v; want the person, suspended", due, err)
	}
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	purged, err := l.h.srv.purgePortalIdentity(ctx, l.st, id, store.PortalPurgeScheduled, portalSweepOrigin())
	if purged || err != nil {
		t.Errorf("purgePortalIdentity of a reactivated person = %v, %v; want false, nil", purged, err)
	}
	ident := l.ident(t, principal)
	if ident.PurgedAt != nil || ident.DeactivatedAt != nil {
		t.Errorf("identity = purged %v, deactivated %v; want active", ident.PurgedAt, ident.DeactivatedAt)
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2`, id, store.JobKindPurge); n != 0 {
		t.Errorf("purge ledger rows = %d, want none", n)
	}
}

// markHook is the store whose purge mark runs before first, once per call, then asks the real store.
type markHook struct {
	store.PG
	calls  int
	before func()
	// result, when set, is what the mark answers instead of asking the store.
	result error
}

func (w *markHook) MarkPortalIdentityPurged(ctx context.Context, id uuid.UUID, mode store.PortalPurgeMode) (bool, error) {
	w.calls++
	if w.before != nil {
		w.before()
	}
	if w.result != nil {
		return false, w.result
	}
	return w.PG.MarkPortalIdentityPurged(ctx, id, mode)
}

// purgeRowsOf is the person.purge rows so far, split by outcome.
func (l *portalLife) purgeRowsOf(t *testing.T, wantReason string) (denied, success int) {
	t.Helper()
	for _, ev := range l.rows("person.purge") {
		switch ev.Outcome {
		case "denied":
			if got := lifeData(ev)["reason"]; got != wantReason {
				t.Errorf("person.purge denial reason = %v, want %s", got, wantReason)
			}
			denied++
		case "success":
			success++
		}
	}
	return denied, success
}

// An explicit purge that finds the person reactivated since the admin read them is refused 409, nothing is
// written, and the request row says denied: no success row is left for a purge that did not happen. The
// reactivation commits across the real store boundary, between the route's reads and the locked mark.
func TestPortalPurgeRoute_RefusedAfterConcurrentReactivate_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d", w.Code)
	}
	id := l.ident(t, principal).ID
	if d, s := l.purgeRowsOf(t, "person_not_suspended"); d != 0 || s != 0 {
		t.Fatalf("control: person.purge rows before the request = %d denied, %d success", d, s)
	}
	hook := &markHook{PG: l.st, before: func() {
		if ok, err := l.st.ReactivatePortalIdentity(ctx, id); err != nil || !ok {
			t.Errorf("reactivate between the read and the mark = %v, %v", ok, err)
		}
	}}
	orig := l.h.srv.cfg.Store
	l.h.srv.cfg.Store = hook
	w := l.post(t, l.admin, principal, "purge", "")
	l.h.srv.cfg.Store = orig
	if hook.calls != 1 {
		t.Fatalf("the mark was reached %d times, want exactly 1 (the route never got to it otherwise: %d %s)", hook.calls, w.Code, w.Body.String())
	}
	if w.Code != http.StatusConflict || errorReason(w) != "person_not_suspended" {
		t.Errorf("purge route = %d %s, want 409 person_not_suspended", w.Code, w.Body.String())
	}
	if d, s := l.purgeRowsOf(t, "person_not_suspended"); d != 1 || s != 0 {
		t.Errorf("person.purge rows after the refused purge = %d denied, %d success; want 1 denied, 0 success", d, s)
	}
	if ident := l.ident(t, principal); ident.PurgedAt != nil || ident.DeactivatedAt != nil {
		t.Errorf("identity = purged %v, deactivated %v; want active", ident.PurgedAt, ident.DeactivatedAt)
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2`, id, store.JobKindPurge); n != 0 {
		t.Errorf("purge ledger rows = %d, want none", n)
	}
}

// An explicit purge whose locked mark finds the row is no longer a bound portal's person is refused 422
// portal_person_only with a denied row: never a person.purge success row, whatever the retry of a purged person does.
func TestPortalPurgeRoute_OwnershipLostLateIsRefused_PG(t *testing.T) {
	l := newPortalLife(t)
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d", w.Code)
	}
	id := l.ident(t, principal).ID
	hook := &markHook{PG: l.st, result: store.ErrNotPortalPerson}
	orig := l.h.srv.cfg.Store
	l.h.srv.cfg.Store = hook
	w := l.post(t, l.admin, principal, "purge", "")
	l.h.srv.cfg.Store = orig
	if hook.calls != 1 {
		t.Fatalf("the mark was reached %d times, want 1 (%d %s)", hook.calls, w.Code, w.Body.String())
	}
	if w.Code != http.StatusUnprocessableEntity || errorReason(w) != "portal_person_only" {
		t.Errorf("purge route = %d %s, want 422 portal_person_only", w.Code, w.Body.String())
	}
	if d, s := l.purgeRowsOf(t, "portal_person_only"); d != 1 || s != 0 {
		t.Errorf("person.purge rows = %d denied, %d success; want 1 denied, 0 success", d, s)
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2`, id, store.JobKindPurge); n != 0 {
		t.Errorf("purge ledger rows = %d, want none", n)
	}
	if ident := l.ident(t, principal); ident.PurgedAt != nil || ident.DeactivatedAt == nil {
		t.Errorf("identity = purged %v, deactivated %v; want suspended and not purged", ident.PurgedAt, ident.DeactivatedAt)
	}
}

// The helper both the sweep and the route share: a reactivated person is ErrPersonNotSuspended in explicit mode and
// left alone (false, nil) in scheduled mode.
func TestPortalPurge_HelperRefusesReactivated_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d", w.Code)
	}
	id := l.ident(t, principal).ID
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	if _, err := l.h.srv.purgePortalIdentity(ctx, l.st, id, store.PortalPurgeExplicit, portalOrigin(types.ActorHuman, "root")); !errors.Is(err, store.ErrPersonNotSuspended) {
		t.Errorf("explicit purge of a reactivated person = %v, want ErrPersonNotSuspended", err)
	}
	if purged, err := l.h.srv.purgePortalIdentity(ctx, l.st, id, store.PortalPurgeScheduled, portalSweepOrigin()); purged || err != nil {
		t.Errorf("scheduled purge of a reactivated person = %v, %v; want false, nil", purged, err)
	}
}

// A crash after the purge's mark and before its steps: with SCIM off the portal sweep finishes the purge.
func TestPortalPurge_CrashResumedWithSCIMOff_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d", w.Code)
	}
	id := l.ident(t, principal).ID
	if marked, err := l.st.MarkPortalIdentityPurged(ctx, id, store.PortalPurgeExplicit); err != nil || !marked {
		t.Fatalf("mark = %v, %v", marked, err)
	}
	pending, err := l.st.PendingPortalLeavers(ctx, scimSweepBatch)
	if err != nil || len(pending) != 1 || !pending[0].Purged {
		t.Fatalf("control: pending leavers = %+v, %v; want the purged person", pending, err)
	}
	if l.h.srv.cfg.SCIM != nil {
		t.Fatal("control: SCIM must be off")
	}
	if err := l.h.srv.SweepPortalLifecycle(ctx); err != nil {
		t.Fatalf("portal sweep: %v", err)
	}
	if !l.allDone(t, id) {
		t.Errorf("ledger not done after the sweep:\n%s", ledgerText(t, l.pool, id))
	}
	if pending, err := l.st.PendingPortalLeavers(ctx, scimSweepBatch); err != nil || len(pending) != 0 {
		t.Errorf("pending leavers after the sweep = %+v, %v", pending, err)
	}
	var purges int
	for _, ev := range l.rows("person.deprovision") {
		if lifeData(ev)["kind"] == store.JobKindPurge && ev.Outcome == "success" {
			purges++
		}
	}
	if purges != 1 {
		t.Errorf("person.deprovision purge rows = %d, want 1", purges)
	}
}

// A purge asked again for a person already purged finishes whatever step is still pending and answers 200.
func TestPortalPurge_ExplicitRetryOnPurgedFinishes_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	l.suspendAndPurge(t, principal)
	id := l.ident(t, principal).ID
	var step string
	if err := l.pool.QueryRow(ctx, `SELECT step FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND step <> $3 ORDER BY step LIMIT 1`,
		id, store.JobKindPurge, store.JobStepAuditDeprovision).Scan(&step); err != nil {
		t.Fatalf("a purge step to reopen: %v", err)
	}
	if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET state = 'pending' WHERE identity_id = $1 AND kind = $2 AND step = $3`, id, store.JobKindPurge, step); err != nil {
		t.Fatal(err)
	}
	if l.allDone(t, id) {
		t.Fatal("control: the reopened step is still done")
	}
	if w := l.post(t, l.admin, principal, "purge", ""); w.Code != http.StatusOK {
		t.Fatalf("retry purge = %d %s, want 200", w.Code, w.Body.String())
	}
	if !l.allDone(t, id) {
		t.Errorf("the retry did not finish step %s:\n%s", step, ledgerText(t, l.pool, id))
	}
}

// A reactivated person signs in again, and their suspension's deadline and cutoff are gone.
func TestPortalReactivate_PG(t *testing.T) {
	l := newPortalLife(t)
	principal, _ := l.signIn(t, "u1")
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend = %d", w.Code)
	}
	if got := l.freshExchange(t, "u1"); got != http.StatusBadRequest {
		t.Fatalf("control: exchange while suspended = %d", got)
	}
	// A security admin may not reactivate.
	if w := l.post(t, l.secAdmin, principal, "reactivate", ""); w.Code != http.StatusForbidden {
		t.Errorf("security admin reactivate = %d, want 403", w.Code)
	}
	w := l.post(t, l.admin, principal, "reactivate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d %s", w.Code, w.Body.String())
	}
	ident := l.ident(t, principal)
	if ident.DeactivatedAt != nil || ident.PurgeAfter != nil || ident.PurgedAt != nil {
		t.Errorf("identity after reactivation = %+v, want active", ident)
	}
	if got := l.freshExchange(t, "u1"); got != http.StatusOK {
		t.Errorf("exchange after the reactivation = %d, want 200", got)
	}
	if rows := l.rows("person.reactivate"); len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Target != principal {
		t.Errorf("person.reactivate rows = %+v, want one success", rows)
	}
}

// A person suspended with a deadline and one suspended without are both reactivated, and the deadline goes with
// the suspension.
func TestPortalReactivate_MarkedWithAndWithoutDeadline_PG(t *testing.T) {
	l := newPortalLife(t)
	for _, c := range []struct{ uid, body string }{{"with", `{"purge_after_seconds": 3600}`}, {"without", ``}} {
		principal, _ := l.signIn(t, c.uid)
		if w := l.post(t, l.admin, principal, "suspend", c.body); w.Code != http.StatusOK {
			t.Fatalf("%s: suspend = %d %s", c.uid, w.Code, w.Body.String())
		}
		if has := l.ident(t, principal).PurgeAfter != nil; has != (c.body != "") {
			t.Fatalf("%s: stored deadline = %v", c.uid, has)
		}
		if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
			t.Fatalf("%s: reactivate = %d %s", c.uid, w.Code, w.Body.String())
		}
		if ident := l.ident(t, principal); ident.PurgeAfter != nil || ident.DeactivatedAt != nil {
			t.Errorf("%s: after reactivation deactivated %v, purge_after %v; want neither", c.uid, ident.DeactivatedAt, ident.PurgeAfter)
		}
	}
}

// Revoking a person's sessions ends their grants but is not a suspension: no deactivation, no ledger, no row of
// the lifecycle's.
func TestPortalRevoke_IsNotSuspend_PG(t *testing.T) {
	l := newPortalLife(t)
	principal, bearer := l.signIn(t, "u1")
	w := doSSO(t, l.h.srv, http.MethodPost, "/api/v1/sessions/revoke", l.admin, `{"sub":"`+principal+`"}`)
	if w.Code >= 300 {
		t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
	}
	if got := l.meCode(t, bearer); got != http.StatusUnauthorized {
		t.Errorf("GET /me after the revoke = %d, want 401", got)
	}
	ident := l.ident(t, principal)
	if ident.DeactivatedAt != nil || ident.PurgedAt != nil {
		t.Errorf("a revoke deactivated the person: %+v", ident)
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1`, ident.ID); n != 0 {
		t.Errorf("deprovision_jobs rows after a revoke = %d, want 0", n)
	}
	for _, a := range []string{"person.suspend", "identity.deactivate", "person.deprovision"} {
		if len(l.rows(a)) != 0 {
			t.Errorf("a revoke wrote %s", a)
		}
	}
}
