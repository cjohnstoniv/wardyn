// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An erasure acts on an entra:<tenant>:<object> form derived from the person's sign-ins only when the
// identity directory proves the form is theirs alone. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const ownIssuerB = "https://sts.windows.net/" + identTenant + "/"

// ownRecords are what one name holds: a credential, a live audit-seal key and a pending governance change.
type ownRecords struct{ change uuid.UUID }

func ownSeed(t *testing.T, srv *Server, pg store.PG, sec secretstore.Store, owner string) ownRecords {
	t.Helper()
	if err := sec.For(owner).Put(t.Context(), "anthropic-api-key", []byte("credential-of-"+owner)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.cfg.SubjectKeys.Current(t.Context(), owner, subjectkey.PurposeAuditSeal); err != nil {
		t.Fatal(err)
	}
	return ownRecords{change: crossProposedChange(t, pg, owner, "")}
}

// ownKept reports which of owner's records survive, as "credential", "key" and "change" flags.
func ownKept(t *testing.T, pg store.PG, sec secretstore.Store, owner string, r ownRecords, since time.Time) (cred, key, change bool) {
	t.Helper()
	destroyed, err := pg.SubjectKeyDestroyedSince(t.Context(), owner, subjectkey.PurposeAuditSeal, since)
	if err != nil {
		t.Fatal(err)
	}
	return len(namesOf(t, sec, owner)) > 0, !destroyed, crossChangeProposer(t, pg, r.change) == owner
}

func ownBind(t *testing.T, pg store.PG, principal, issuer, oid string) store.PrincipalIdentity {
	t.Helper()
	id, err := pg.UpsertLoginIdentity(t.Context(), store.LoginIdentity{Principal: principal, Issuer: issuer, TenantID: identTenant, ObjectID: oid}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ownPerson(t *testing.T, pg store.PG, principal, issuer, oid string) {
	t.Helper()
	if _, _, err := pg.CreatePerson(t.Context(), types.Person{Principal: principal, Issuer: issuer, TenantID: identTenant, ObjectID: oid, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
}

func ownDeactivate(t *testing.T, pg store.PG, oid string) {
	t.Helper()
	if _, err := pg.Pool.Exec(t.Context(), `UPDATE principal_identities SET deactivated_at = now() WHERE object_id = $1`, oid); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(t.Context(), `UPDATE people SET deactivated_at = now() WHERE object_id = $1`, oid); err != nil {
		t.Fatal(err)
	}
}

func ownErase(t *testing.T, srv *Server, method, name string) (code int, reason string) {
	t.Helper()
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	path, body := "/api/v1/people/"+name+"/erasure", erasureBody("audit_personal_fields", "credentials")
	if method == http.MethodDelete {
		path, body = "/api/v1/people/"+name+"/credentials", ""
	}
	w := doSSO(t, srv, method, path, sa, body)
	return w.Code, errorReason(w)
}

// P is bound to entra:<tenant>:<object> under one issuer, Y is a pairwise subject with the same tenant and
// object id under another. Erasing Y never reaches P's credential, audit key or governance change, and
// erasing P never reaches Y's, active or deactivated.
func TestPG_Erasure_AnotherIssuersEntraNameIsNeverErased(t *testing.T) {
	for _, deactivated := range []bool{false, true} {
		for _, erasingY := range []bool{true, false} {
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				t.Run(fmt.Sprintf("deactivated=%v/erasingY=%v/%s", deactivated, erasingY, method), func(t *testing.T) {
					_, srv, pg, sec := identityRig(t)
					oid := uuid.NewString()
					p, y := "entra:"+identTenant+":"+oid, "pairwise-y-"+oid
					ownPerson(t, pg, p, identIssuer, oid)
					ownBind(t, pg, p, identIssuer, oid)
					ownBind(t, pg, y, ownIssuerB, oid)
					if deactivated {
						ownDeactivate(t, pg, oid)
					}
					target, other := y, p
					if !erasingY {
						target, other = p, y
					}
					since := time.Now().UTC().Add(-time.Second)
					targetRecs, otherRecs := ownSeed(t, srv, pg, sec, target), ownSeed(t, srv, pg, sec, other)
					if code, reason := ownErase(t, srv, method, target); code != http.StatusOK {
						t.Fatalf("erasing %q = %d %s, want 200", target, code, reason)
					}
					if cred, _, _ := ownKept(t, pg, sec, target, targetRecs, since); cred {
						t.Errorf("erasing %q left its credential", target)
					}
					if cred, key, change := ownKept(t, pg, sec, other, otherRecs, since); !cred || !key || !change {
						t.Errorf("erasing %q reached %q: credential kept %v, key kept %v, change kept %v", target, other, cred, key, change)
					}
				})
			}
		}
	}
}

// A person an admin created by object id who signed in under a pairwise subject of the same issuer may
// still hold records under the entra:<tenant>:<object> name. Erasing the subject erases them, active or
// deactivated.
func TestPG_Erasure_SameIssuerHistoryUnderTheEntraNameIsErased(t *testing.T) {
	for _, deactivated := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "deactivated"}[deactivated], func(t *testing.T) {
			_, srv, pg, sec := identityRig(t)
			oid := uuid.NewString()
			c, y := "entra:"+identTenant+":"+oid, "pairwise-"+oid
			ownPerson(t, pg, c, identIssuer, oid)
			ownBind(t, pg, y, identIssuer, oid)
			if deactivated {
				ownDeactivate(t, pg, oid)
			}
			since := time.Now().UTC().Add(-time.Second)
			history, own := ownSeed(t, srv, pg, sec, c), ownSeed(t, srv, pg, sec, y)
			if code, reason := ownErase(t, srv, http.MethodPost, y); code != http.StatusOK {
				t.Fatalf("erasing %q = %d %s, want 200", y, code, reason)
			}
			for name, r := range map[string]ownRecords{c: history, y: own} {
				if cred, key, change := ownKept(t, pg, sec, name, r, since); cred || key || change {
					t.Errorf("erasing %q left %q: credential %v, key %v, change %v", y, name, cred, key, change)
				}
			}
		})
	}
}

// Y and Q are pairwise subjects with one tenant and object id under two issuers, and nobody is bound to
// the entra:<tenant>:<object> name c: what is under c may be either's. Erasing Y erases Y's own records on
// every route (POST /erasure, DELETE /credentials, the SCIM purge's erase), leaves c's alone, and names c
// and Q as not erased.
func TestPG_Erasure_AnAmbiguousEntraNameIsLeftAndListed(t *testing.T) {
	for _, route := range []string{"erasure", "credentials", "scim purge"} {
		t.Run(route, func(t *testing.T) {
			_, srv, pg, sec := identityRig(t)
			oid := uuid.NewString()
			c, y, q := "entra:"+identTenant+":"+oid, "pairwise-y-"+oid, "pairwise-q-"+oid
			ownBind(t, pg, y, ownIssuerB, oid)
			ownBind(t, pg, q, identIssuer, oid)
			since := time.Now().UTC().Add(-time.Second)
			history, own := ownSeed(t, srv, pg, sec, c), ownSeed(t, srv, pg, sec, y)
			sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
			var w *httptest.ResponseRecorder
			switch route {
			case "erasure":
				w = doSSO(t, srv, http.MethodPost, "/api/v1/people/"+y+"/erasure", sa, erasureBody("audit_personal_fields", "credentials"))
			case "credentials":
				w = doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+y+"/credentials", sa, "")
			default:
				if _, err := srv.eraseForPurge(t.Context(), y); err != nil {
					t.Fatalf("purging %q = %v, want done", y, err)
				}
			}
			if w != nil {
				var body struct {
					NotErased struct{ Subjects []string } `json:"not_erased"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); w.Code != http.StatusOK || err != nil || !slices.Equal(body.NotErased.Subjects, []string{c, q}) {
					t.Fatalf("erasing %q = %d %s, want 200 naming %q and %q not erased", y, w.Code, w.Body, c, q)
				}
			}
			cred, key, change := ownKept(t, pg, sec, y, own, since)
			if cred || (route == "erasure" && (key || change)) {
				t.Errorf("erasing %q left its own: credential %v, key %v, change %v", y, cred, key, change)
			}
			if cred, key, change := ownKept(t, pg, sec, c, history, since); !cred || !key || !change {
				t.Errorf("erasing %q reached %q: credential kept %v, key kept %v, change kept %v", y, c, cred, key, change)
			}
		})
	}
}

// otherIssuersPrincipal is P: another issuer's sign-in bound to the principal p, with a credential, a
// workspace and a grant of its own.
type otherIssuersPrincipal struct {
	p, issuer string
	ws        uuid.UUID
}

func (e *scimEnv) seedOtherIssuersPrincipal(p, oid string) otherIssuersPrincipal {
	e.t.Helper()
	ctx := context.Background()
	o := otherIssuersPrincipal{p: p, issuer: "https://other-issuer.example/v2.0"}
	if _, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: o.issuer, TenantID: e.tenant, ObjectID: oid}, time.Now().UTC()); err != nil {
		e.t.Fatal(err)
	}
	if err := e.sec.For(p).Put(ctx, "api-key", []byte("p-secret")); err != nil {
		e.t.Fatal(err)
	}
	ws, err := e.st.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: "ws-p", OwnedBy: p, Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{SubjectType: types.CapabilitySubjectUser, Subject: p,
		Capability: "egress_domain", Value: "evil.example", Effect: types.CapabilityDeny}); err != nil {
		e.t.Fatal(err)
	}
	o.ws = ws.ID
	return o
}

// kept checks P's credential, workspace and grant are all still there, and that P's identity is neither
// scheduled for purge nor purged.
func (e *scimEnv) kept(o otherIssuersPrincipal, oid, when string) {
	e.t.Helper()
	e.keptRecords(o, when)
	if id, err := e.st.GetIdentityByObject(context.Background(), o.issuer, e.tenant, oid); err != nil || id.PurgeAfter != nil || id.PurgedAt != nil {
		e.t.Errorf("%s: %q's identity = %+v (%v), want it neither scheduled for purge nor purged", when, o.p, id, err)
	}
}

// keptRecords checks P's credential, workspace and grant are all still there.
func (e *scimEnv) keptRecords(o otherIssuersPrincipal, when string) {
	e.t.Helper()
	if n := e.credentialCount(o.p); n != 1 {
		e.t.Errorf("%s: %q holds %d credentials, want its own kept", when, o.p, n)
	}
	if got, err := e.st.GetWorkspace(context.Background(), o.ws); err != nil || got.OwnedBy != o.p {
		e.t.Errorf("%s: %q's workspace is owned by %q (%v), want it kept", when, o.p, got.OwnedBy, err)
	}
	if g, _ := e.userRows(o.p); g != 1 {
		e.t.Errorf("%s: %q holds %d grants, want its own kept", when, o.p, g)
	}
}

// sweepPastThePurgeDelay runs the purge sweeper as if every scheduled purge were due and every pending
// ledger row idle.
func (e *scimEnv) sweepPastThePurgeDelay() error {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET purge_after = now() - interval '1 minute' WHERE purge_after IS NOT NULL`); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour'`); err != nil {
		e.t.Fatal(err)
	}
	return e.b.srv.SweepSCIMPurge(ctx)
}

// A SCIM leaver Y whose entra:<tenant>:<object> form is bound to another issuer's principal P: the purge
// never reassigns P's workspace, deletes P's grant or erases P's credential, and its row names P as not
// erased. The suspension deactivates P's binding but never schedules it for purge, so the sweeper, past
// the purge delay, leaves P alone too.
func TestSCIMPurgeLeavesAnotherIssuersEntraPrincipal(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = time.Hour })
	ctx := t.Context()
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	o := e.seedOtherIssuersPrincipal("entra:"+e.tenant+":"+purgeOID, purgeOID)

	if w := e.del(e.a, f.id); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
	}
	if n := e.credentialCount(purgeSub); n != 0 {
		t.Errorf("the leaver kept %d credentials", n)
	}
	e.kept(o, purgeOID, "after the purge")
	// The suspension the purge begins with is the leaver's cutoff and cuts the form SCIM names, P's binding
	// included (TestSCIMSuspendReachesEveryForm); only the destructive steps above are proved.
	if id, err := e.st.GetIdentityByObject(ctx, o.issuer, e.tenant, purgeOID); err != nil || id.DeactivatedAt == nil {
		t.Errorf("%q's identity = %+v (%v), want it deactivated by the suspension", o.p, id, err)
	}
	rows := e.purgeRows(e.a, e.b)
	if len(rows) != 1 {
		t.Fatalf("%d purge rows, want 1", len(rows))
	}
	if ne, _ := dataOf(t, rows[0])["not_erased"].(map[string]any); fmt.Sprint(ne["subjects"]) != "["+o.p+"]" {
		t.Errorf("purge row not_erased = %v, want [%s]", ne, o.p)
	}

	if err := e.sweepPastThePurgeDelay(); err != nil {
		t.Errorf("sweep: %v", err)
	}
	e.kept(o, purgeOID, "after the sweeper")
}

// A schedule an earlier release left on P's identity, deactivated alongside the SCIM leaver Y whose
// entra:<tenant>:<object> form is P's principal: P has no suspension of its own on the ledger, so the
// sweeper past the purge delay leaves P's identity, credential, workspace and grant alone, and still purges
// Y on schedule.
func TestSCIMSweeperLeavesAScheduleWithNoSuspensionOfItsOwn(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = time.Hour })
	ctx := t.Context()
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	o := e.seedOtherIssuersPrincipal("entra:"+e.tenant+":"+purgeOID, purgeOID)
	e.suspendOn(e.a, purgeEmail)
	if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET purge_after = now() - interval '1 minute' WHERE issuer = $1`, o.issuer); err != nil {
		t.Fatal(err)
	}
	p, err := e.st.GetIdentityByObject(ctx, o.issuer, e.tenant, purgeOID)
	if err != nil || p.DeactivatedAt == nil || p.PurgeAfter == nil {
		t.Fatalf("control: %q's identity = %+v (%v), want it deactivated and scheduled", o.p, p, err)
	}
	if jobs, err := e.st.ListDeprovisionJobs(ctx, p.ID, store.JobKindSuspend); err != nil || len(jobs) != 0 {
		t.Fatalf("control: %q's suspension ledger = %+v (%v), want none of its own", o.p, jobs, err)
	}

	if err := e.sweepPastThePurgeDelay(); err != nil {
		t.Errorf("sweep: %v", err)
	}
	e.keptRecords(o, "after the sweeper")
	if p, err = e.st.GetIdentity(ctx, p.ID); err != nil || p.PurgedAt != nil {
		t.Errorf("%q's identity = %+v (%v), want it not purged", o.p, p, err)
	}
	if row := e.identity(f.id); row.PurgedAt == nil {
		t.Errorf("the leaver's identity = %+v, want it purged on schedule", row)
	}
	if n := e.credentialCount(purgeSub); n != 0 {
		t.Errorf("the leaver kept %d credentials", n)
	}
}

// A SCIM row no sign-in bound, whose entra:<tenant>:<object> form is another issuer's principal P: the
// suspension cuts the form, and the purge refuses its destructive steps, so P's credential, workspace and
// grant survive the DELETE and the sweeper.
func TestSCIMPurgeOfAnUnboundRowLeavesAnotherIssuersPrincipalItsFormNames(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = time.Hour })
	o := e.seedOtherIssuersPrincipal("entra:"+e.tenant+":"+purgeOID, purgeOID)
	id := e.postUserID(e.a, purgeOID, purgeEmail, purgeEmail)
	if row := e.identity(id); row.Principal != "" || row.Issuer == o.issuer {
		t.Fatalf("control: the SCIM row is %+v, want an unbound row of the SCIM issuer", row)
	}

	if w := e.del(e.a, id); w.Code != http.StatusInternalServerError {
		t.Errorf("DELETE = %d %s, want 500 (the purge refuses)", w.Code, w.Body.String())
	}
	e.kept(o, purgeOID, "after the DELETE")
	if row, err := e.st.GetIdentityByObject(t.Context(), o.issuer, e.tenant, purgeOID); err != nil || row.DeactivatedAt == nil {
		t.Errorf("%q's identity = %+v (%v), want it deactivated by the suspension", o.p, row, err)
	}
	rows := e.purgeRows(e.a, e.b)
	if len(rows) != 1 || rows[0].Outcome != "denied" || dataOf(t, rows[0])["unproved"] != o.p {
		t.Fatalf("purge rows = %+v, want one denied row naming %s unproved", rows, o.p)
	}

	if err := e.sweepPastThePurgeDelay(); err == nil {
		t.Error("the sweeper finished a purge that refuses")
	}
	e.kept(o, purgeOID, "after the sweeper")
}

// The purge's refusal of a form it cannot prove is held on the deprovision ledger, so /scim/status lists
// it, and a sweep that finds it there again writes no second denied row.
func TestSCIMPurgeRefusalIsOnTheLedgerOnce(t *testing.T) {
	e := newSCIMEnv(t)
	ctx := t.Context()
	e.seedOtherIssuersPrincipal("entra:"+e.tenant+":"+purgeOID, purgeOID)
	id := e.postUserID(e.a, purgeOID, purgeEmail, purgeEmail)
	w := e.del(e.a, id)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE = %d %s, want 500 (the purge refuses)", w.Code, w.Body.String())
	}
	if b := w.Body.String(); !strings.Contains(b, "a retry does not finish it") || strings.Contains(b, "retry the request") {
		t.Errorf("DELETE body = %s, want it to say a retry does not finish the purge", b)
	}
	failures := func(when string) {
		t.Helper()
		got, err := e.st.ListDeprovisionFailures(ctx, 10)
		if err != nil || len(got) != 1 || got[0].IdentityID.String() != id || got[0].Kind != store.JobKindPurge ||
			got[0].Step != store.JobStepAuditDeprovision || !strings.Contains(got[0].LastError, "binds the SCIM user to the person's sign-in") ||
			strings.Contains(got[0].LastError, "retry the request") {
			t.Errorf("%s: deprovision failures = %+v (%v), want the one refusal on the purge's audit step, naming what an admin does", when, got, err)
		}
	}
	failures("after the DELETE")

	for range 2 {
		if err := e.sweepPastThePurgeDelay(); err == nil {
			t.Error("the sweeper finished a purge that refuses")
		}
	}
	failures("after two sweeps")
	if rows := e.purgeRows(e.a, e.b); len(rows) != 1 || rows[0].Outcome != "denied" {
		t.Errorf("%d purge rows (%+v), want the one denied row", len(rows), rows)
	}
}

// The names are proved once, before anything runs; a binding that changes before a destructive step fails
// that step and erases nothing under the name it changed.
func TestPG_Erasure_ABindingThatChangesAfterTheProofFailsTheStep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, pg store.PG, oid string) string // binds Y, returns Y
		change func(t *testing.T, pg store.PG, oid, y string)
	}{
		{name: "another issuer's sign-in binds the entra name",
			before: func(t *testing.T, pg store.PG, oid string) string {
				ownBind(t, pg, "pairwise-y-"+oid, ownIssuerB, oid)
				return "pairwise-y-" + oid
			},
			change: func(t *testing.T, pg store.PG, oid, _ string) {
				c := "entra:" + identTenant + ":" + oid
				ownPerson(t, pg, c, identIssuer, oid)
				ownBind(t, pg, c, identIssuer, oid)
			}},
		{name: "the person's sign-in is released",
			before: func(t *testing.T, pg store.PG, oid string) string {
				ownPerson(t, pg, "entra:"+identTenant+":"+oid, identIssuer, oid)
				ownBind(t, pg, "pairwise-"+oid, identIssuer, oid)
				return "pairwise-" + oid
			},
			change: func(t *testing.T, pg store.PG, oid, y string) {
				id, err := pg.GetIdentityByObject(t.Context(), identIssuer, identTenant, oid)
				if err != nil {
					t.Fatal(err)
				}
				if err := pg.UnbindIdentity(t.Context(), id.ID, y); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		for _, sc := range []erasure.Scope{erasure.AuditPersonalFields, erasure.Credentials} {
			t.Run(tc.name+"/"+string(sc), func(t *testing.T) {
				_, srv, pg, sec := identityRig(t)
				oid := uuid.NewString()
				c, y := "entra:"+identTenant+":"+oid, tc.before(t, pg, oid)
				want, err := srv.erasureNamesOf(t.Context(), y)
				if err != nil || len(want.entra) != 1 || want.entra[0] != c {
					t.Fatalf("proved names of %q = %+v, %v, want the entra name %q", y, want, err, c)
				}
				tc.change(t, pg, oid, y)
				since := time.Now().UTC().Add(-time.Second)
				history := ownSeed(t, srv, pg, sec, c)
				_, err = srv.erasureOrchestrator(nil, &want).Orchestrate(t.Context(), y, []erasure.Scope{sc})
				if !errors.Is(err, errErasureNamesChanged) {
					t.Fatalf("erasing %q after the change = %v, want %v", y, err, errErasureNamesChanged)
				}
				if cred, key, _ := ownKept(t, pg, sec, c, history, since); !cred || !key {
					t.Errorf("a step that failed on the change reached %q: credential kept %v, key kept %v", c, cred, key)
				}
			})
		}
	}
}

// Erasing either of P (bound to entra:<tenant>:<object> under one issuer) and Y (a pairwise subject with
// the same ids under another) leaves the other alone, and the response and the audit row name the other by
// its exact subject, to be erased by subject.
func TestPG_Erasure_NamesTheOtherIssuersPrincipalNotErased(t *testing.T) {
	for _, erasingY := range []bool{true, false} {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			t.Run(fmt.Sprintf("erasingY=%v/%s", erasingY, method), func(t *testing.T) {
				h, srv, pg, _ := identityRig(t)
				oid := uuid.NewString()
				p, y := "entra:"+identTenant+":"+oid, "pairwise-y-"+oid
				ownPerson(t, pg, p, identIssuer, oid)
				ownBind(t, pg, p, identIssuer, oid)
				ownBind(t, pg, y, ownIssuerB, oid)
				target, other := y, p
				if !erasingY {
					target, other = p, y
				}
				sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
				path, body, action := "/api/v1/people/"+target+"/erasure", erasureBody("run_tasks"), "person.erasure"
				if method == http.MethodDelete {
					path, body, action = "/api/v1/people/"+target+"/credentials", "", "credential.erase"
				}
				w := doSSO(t, srv, method, path, sa, body)
				var resp struct {
					NotErased struct {
						Subjects []string
						Note     string
					} `json:"not_erased"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); w.Code != http.StatusOK || err != nil ||
					!slices.Equal(resp.NotErased.Subjects, []string{other}) || resp.NotErased.Note != "not erased: erase by subject" {
					t.Fatalf("erasing %q = %d %s, want 200 naming %q not erased", target, w.Code, w.Body, other)
				}
				var row *types.AuditEvent
				for i, ev := range h.audit.events {
					if ev.Action == action && ev.Outcome == "success" {
						row = &h.audit.events[i]
					}
				}
				if row == nil || !strings.Contains(string(row.Data), `"not_erased":{"note":"not erased: erase by subject","subjects":["`+other+`"]}`) {
					t.Fatalf("the %s row = %+v, want it to name %q not erased", action, row, other)
				}
			})
		}
	}
}
