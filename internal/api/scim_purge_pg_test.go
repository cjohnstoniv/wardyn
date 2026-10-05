// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Purge: DELETE and the purge sweeper, over a real Postgres with two instances. The subject of every test
// here is a person the identity provider has suspended or deleted.

const (
	purgeSub   = "sub-purge"
	purgeEmail = "purge.me@corp.example"
	purgeOID   = "aaaaaaaa-0000-4000-8000-0000000000b1"
)

// purgeFixture is everything a purge reaches for one person, seeded under their sub and email.
type purgeFixture struct {
	id        string
	workspace uuid.UUID
	drive     string
}

func (e *scimEnv) seedPurgeSubject(sub, email, oid string) purgeFixture {
	e.t.Helper()
	ctx := context.Background()
	e.seedEntra(sub, email, oid)
	f := purgeFixture{id: e.postUserID(e.a, oid, email, email)}
	if err := e.sec.For(sub).Put(ctx, "api-key", []byte("s3cret")); err != nil {
		e.t.Fatal(err)
	}
	ws, err := e.st.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "ws-" + sub, OwnedBy: sub, Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	f.workspace = ws.ID
	// A deny grant and a deny-by-profile assignment: the rows whose deletion GOV4 would call widening.
	for _, subject := range []string{sub, email} {
		if _, err := e.st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: subject, Capability: "egress_domain", Value: "evil.example", Effect: types.CapabilityDeny,
		}); err != nil {
			e.t.Fatal(err)
		}
	}
	prof, err := e.st.UpsertGovernanceProfile(ctx, types.GovernanceProfile{ID: uuid.New(), Name: "walled-" + sub, Ceiling: govProfileSpec()})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: sub, ProfileID: prof.ID,
	}); err != nil {
		e.t.Fatal(err)
	}
	// A group row and another person's user row must survive.
	if _, err := e.st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: "someone-else", Capability: "egress_domain", Value: "evil.example", Effect: types.CapabilityDeny,
	}); err != nil {
		e.t.Fatal(err)
	}
	drive, err := e.st.UpsertUserDrive(ctx, *driveFixture(func(d *types.UserDrive) { d.Name = "Corp NAS " + sub }), false)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.UpsertUserDriveGrant(ctx, *grantFixture(drive.ID, func(g *types.UserDriveGrant) { g.Subject = sub }), false); err != nil {
		e.t.Fatal(err)
	}
	f.drive = drive.Name
	return f
}

func (e *scimEnv) del(n *scimNode, id string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.scim(n, http.MethodDelete, "/scim/v2/Users/"+id, "")
}

func (e *scimEnv) purgeRows(nodes ...*scimNode) []types.AuditEvent {
	e.t.Helper()
	var out []types.AuditEvent
	for _, n := range nodes {
		for _, ev := range e.rows(n, "person.deprovision") {
			if dataOf(e.t, ev)["kind"] == store.JobKindPurge {
				out = append(out, ev)
			}
		}
	}
	return out
}

func (e *scimEnv) credentialCount(owner string) int {
	e.t.Helper()
	names, err := e.sec.For(owner).List(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return len(names)
}

func (e *scimEnv) userRows(subject string) (grants, assignments int) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM capability_grants WHERE subject_type = 'user' AND lower(subject) = lower($1)`, subject).Scan(&grants); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM governance_assignments WHERE subject_type = 'user' AND lower(subject) = lower($1)`, subject).Scan(&assignments); err != nil {
		e.t.Fatal(err)
	}
	return grants, assignments
}

// DELETE suspends, erases through the orchestrator with the credentials and mask_copies scopes,
// reassigns workspaces, deletes the user-subject grants and assignments and lists the drives; then the
// identity is a tombstone that cannot be reactivated or sign in. A repeat DELETE changes nothing.
func TestSCIMDeletePurgesAndTombstones(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	run := e.seedRun(purgeSub, types.RunRunning)
	other := e.seedPurgeSubject("sub-bystander", "bystander@corp.example", "aaaaaaaa-0000-4000-8000-0000000000b2")

	if w := e.del(e.b, f.id); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("DELETE = %d %q, want 204", w.Code, w.Body.String())
	}
	row := e.identity(f.id)
	if row.PurgedAt == nil || row.DeactivatedAt == nil {
		t.Fatalf("identity after DELETE: %+v, want deactivated and purged", row)
	}
	if e.runState(run) != types.RunKilled {
		t.Errorf("run state = %s, want the suspension's kill", e.runState(run))
	}
	if n := e.credentialCount(purgeSub); n != 0 {
		t.Errorf("%d credentials left under the purged person", n)
	}
	if n := e.credentialCount("sub-bystander"); n != 1 {
		t.Errorf("the bystander holds %d credentials, want 1", n)
	}
	ws, err := e.st.GetWorkspace(context.Background(), f.workspace)
	if err != nil || ws.OwnedBy != "" {
		t.Errorf("workspace owner = %q, %v, want the operator", ws.OwnedBy, err)
	}
	if g, a := e.userRows(purgeSub); g != 0 || a != 0 {
		t.Errorf("sub rows left: %d grants, %d assignments", g, a)
	}
	if g, _ := e.userRows(purgeEmail); g != 0 {
		t.Errorf("%d email-subject grants left", g)
	}
	if g, a := e.userRows("sub-bystander"); g != 1 || a != 1 {
		t.Errorf("the bystander's rows = %d grants, %d assignments, want one each", g, a)
	}
	if g, _ := e.userRows("someone-else"); g != 1 {
		t.Errorf("an unrelated person's grant was deleted")
	}

	rows := e.purgeRows(e.a, e.b)
	if len(rows) != 1 {
		t.Fatalf("%d purge person.deprovision rows, want 1", len(rows))
	}
	d := dataOf(t, rows[0])
	drives, _ := d["drives"].([]any)
	if d["credentials_erased"] != float64(1) || d["workspaces_reassigned"] != float64(1) || d["grants_deleted"] != float64(2) ||
		d["assignments_deleted"] != float64(1) || len(drives) != 1 || drives[0] != f.drive {
		t.Errorf("purge row = %v, want 1 credential, 1 workspace, 2 grants, 1 assignment, drive %q", d, f.drive)
	}
	if got := e.rows(e.b, "person.deprovision"); len(got) != 2 {
		t.Errorf("%d person.deprovision rows on the serving node, want the suspension's and the purge's", len(got))
	}
	if drivesLeft, err := e.st.ListUserDriveGrants(context.Background()); err != nil || len(drivesLeft) != 2 {
		t.Errorf("drive grants = %d, %v: a purge lists drives and never reclaims them", len(drivesLeft), err)
	}

	// Tombstone: not reactivable, not signed-in.
	if w := e.patch(e.a, f.id, patchOf("true")); w.Code != http.StatusBadRequest {
		t.Errorf("PATCH active=true after DELETE = %d, want 400", w.Code)
	}
	if su := e.signIn(e.a, purgeSub, purgeEmail); sessionCookieOf(su) != nil || authErrorOf(su) != "sign_in_refused" {
		t.Errorf("the purged person signed in: %d", su.Code)
	}
	// A repeat DELETE is a 204 that writes nothing.
	if w := e.del(e.a, f.id); w.Code != http.StatusNoContent {
		t.Errorf("repeat DELETE = %d", w.Code)
	}
	if len(e.purgeRows(e.a, e.b)) != 1 {
		t.Error("a repeat DELETE wrote another purge row")
	}
	_ = other
}

func TestSCIMDeleteUnknownIsNotFound(t *testing.T) {
	e := newSCIMEnv(t)
	if w := e.del(e.a, uuid.NewString()); w.Code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown id = %d, want 404", w.Code)
	}
}

// WARDYN_SCIM_LEAVER_WORKSPACES set to keep leaves the workspaces with the purged person.
func TestSCIMPurgeKeepsWorkspacesWhenAsked(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.KeepWorkspaces = true })
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	if w := e.del(e.a, f.id); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	if ws, err := e.st.GetWorkspace(context.Background(), f.workspace); err != nil || ws.OwnedBy != purgeSub {
		t.Errorf("workspace owner = %q, %v, want it kept", ws.OwnedBy, err)
	}
	if d := dataOf(t, e.purgeRows(e.a)[0]); d["workspaces_reassigned"] != float64(0) {
		t.Errorf("purge row = %v", d)
	}
}

// ageSuspension makes a suspended identity due for purge.
func (e *scimEnv) ageSuspension(id string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_identities SET purge_after = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
}

// Suspension schedules the purge WARDYN_SCIM_PURGE_AFTER ahead, and a reactivation clears it.
func TestSCIMSuspensionSchedulesThePurge(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = 48 * time.Hour })
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	e.suspendOn(e.a, purgeEmail)
	row := e.identity(f.id)
	if row.PurgeAfter == nil || row.DeactivatedAt == nil {
		t.Fatalf("suspended identity: %+v", row)
	}
	if d := row.PurgeAfter.Sub(*row.DeactivatedAt); d < 47*time.Hour || d > 49*time.Hour {
		t.Errorf("purge_after is %s after the suspension, want 48h", d)
	}
	if w := e.patch(e.a, f.id, patchOf("true")); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	if e.identity(f.id).PurgeAfter != nil {
		t.Error("reactivation left a purge schedule")
	}
}

// Two instances sweep at once over a due identity: one purge, one purge row. And two instances holding
// the same stale candidate: the row lock lets exactly one of them purge.
func TestSCIMSweeperPurgesOnceAcrossInstances(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = time.Hour })
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	e.suspendOn(e.a, purgeEmail)
	e.ageSuspension(f.id)
	racer := e.seedPurgeSubject("sub-racer", "racer@corp.example", "aaaaaaaa-0000-4000-8000-0000000000b3")
	e.suspendOn(e.a, "racer@corp.example")
	e.ageSuspension(racer.id)

	var won int32
	var wg sync.WaitGroup
	for _, n := range []*scimNode{e.a, e.b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			purged, err := n.srv.purgeIdentity(context.Background(), e.st, uuid.MustParse(racer.id), scimSweeperSlot, true)
			if err != nil {
				t.Errorf("purge: %v", err)
			}
			if purged {
				atomic.AddInt32(&won, 1)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d instances purged the same candidate, want exactly 1", won)
	}

	for _, n := range []*scimNode{e.a, e.b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := n.srv.SweepSCIMPurge(context.Background()); err != nil {
				t.Errorf("sweep: %v", err)
			}
		}()
	}
	wg.Wait()
	for _, id := range []string{f.id, racer.id} {
		if e.identity(id).PurgedAt == nil {
			t.Fatalf("identity %s is not purged", id)
		}
	}
	if rows := e.purgeRows(e.a, e.b); len(rows) != 2 {
		t.Errorf("%d purge rows across two instances for two people, want exactly 2", len(rows))
	}
	if e.credentialCount(purgeSub) != 0 || e.credentialCount("sub-racer") != 0 {
		t.Error("credentials left after the purge")
	}
	// Another tick finds nothing to do.
	if err := e.a.srv.SweepSCIMPurge(context.Background()); err != nil || len(e.purgeRows(e.a, e.b)) != 2 {
		t.Errorf("a later sweep = %v, %d purge rows", err, len(e.purgeRows(e.a, e.b)))
	}
}

// A reactivation that commits after the sweeper's candidate read and before its purge wins: the person
// is left unpurged, with nothing erased.
func TestSCIMSweeperSkipsAReactivatedCandidate(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = time.Hour })
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	e.suspendOn(e.a, purgeEmail)
	e.ageSuspension(f.id)

	ctx := context.Background()
	due, err := e.st.PurgeDueIdentities(ctx, 10)
	if err != nil || len(due) != 1 || due[0].String() != f.id {
		t.Fatalf("candidates = %v, %v", due, err)
	}
	if w := e.patch(e.b, f.id, patchOf("true")); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	purged, err := e.a.srv.purgeIdentity(ctx, e.st, due[0], scimSweeperSlot, true)
	if err != nil || purged {
		t.Fatalf("purgeIdentity of a reactivated candidate = %v, %v, want skipped", purged, err)
	}
	if e.identity(f.id).PurgedAt != nil || e.credentialCount(purgeSub) != 1 || len(e.purgeRows(e.a, e.b)) != 0 {
		t.Error("a reactivated person was purged")
	}
}

// WARDYN_SCIM_PURGE_AFTER of zero purges nothing automatically, and DELETE still purges.
func TestSCIMSweeperDoesNotPurgeWhenAutoPurgeIsOff(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = 0 })
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	e.suspendOn(e.a, purgeEmail)
	if e.identity(f.id).PurgeAfter != nil {
		t.Fatal("a zero delay scheduled a purge")
	}
	e.ageSuspension(f.id)
	if err := e.a.srv.SweepSCIMPurge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.identity(f.id).PurgedAt != nil {
		t.Fatal("the sweeper purged with the automatic purge off")
	}
	if w := e.del(e.a, f.id); w.Code != http.StatusNoContent || e.identity(f.id).PurgedAt == nil {
		t.Errorf("DELETE with the automatic purge off = %d", w.Code)
	}
}

// A suspension that failed and that the identity provider never retried is finished by the sweeper once
// its ledger has sat idle; a ledger touched a moment ago is left to the request still working on it.
func TestSCIMSweeperResumesAPendingSuspension(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	run := e.seedRun(purgeSub, types.RunRunning)
	e.a.runner.setFailKills(1000)
	if w := e.patch(e.a, f.id, patchOf("false")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspend with a failing runner = %d, want 5xx", w.Code)
	}
	e.a.runner.setFailKills(0)
	ctx := context.Background()
	if err := e.b.srv.SweepSCIMPurge(ctx); err != nil {
		t.Fatal(err)
	}
	if e.allJobsDone(f.id) {
		t.Fatal("the sweeper raced a ledger touched a moment ago")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour' WHERE identity_id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	if err := e.b.srv.SweepSCIMPurge(ctx); err != nil {
		t.Fatal(err)
	}
	if !e.allJobsDone(f.id) || e.runState(run) != types.RunKilled {
		t.Errorf("pending suspension not completed: done=%v run=%s", e.allJobsDone(f.id), e.runState(run))
	}
	if got := e.rows(e.b, "scim.user.deactivate"); len(got) != 1 {
		t.Errorf("%d scim.user.deactivate rows after the resume, want 1", len(got))
	}
}

// A purge that failed part way is finished by the sweeper, and writes its one row then.
func TestSCIMSweeperResumesAnIncompletePurge(t *testing.T) {
	e := newSCIMEnv(t)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	e.a.audit.fail("person.deprovision", true)
	if w := e.del(e.a, f.id); w.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE with a failing audit sink = %d, want 5xx", w.Code)
	}
	if row := e.identity(f.id); row.PurgedAt == nil {
		t.Fatal("the failed purge was not marked: a reactivation could still win")
	}
	if w := e.patch(e.b, f.id, patchOf("true")); w.Code != http.StatusBadRequest {
		t.Errorf("reactivation during an unfinished purge = %d, want 400", w.Code)
	}
	e.a.audit.fail("person.deprovision", false)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour' WHERE identity_id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	if err := e.b.srv.SweepSCIMPurge(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := e.purgeRows(e.a, e.b); len(rows) != 1 {
		t.Errorf("%d purge rows after the resume, want 1", len(rows))
	}
}

// An email alias another principal holds is not the leaver's to clear: the purge keeps that email-keyed deny
// row (it can be the recycled address's new holder's), deletes every row keyed by the leaver's own
// principals, and names the kept rows in person.deprovision.
func TestSCIMPurgeKeepsEmailRowsAnotherPrincipalHolds(t *testing.T) {
	cases := map[string]func(e *scimEnv){
		"a token minted by another principal under the address": func(e *scimEnv) { e.seedToken("sub-newhire", purgeEmail) },
		"another active identity seen under the address":        func(e *scimEnv) { e.seedSignIn("sub-newhire", purgeEmail) },
	}
	for name, holder := range cases {
		t.Run(name, func(t *testing.T) {
			e := newSCIMEnv(t)
			f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
			holder(e)

			if w := e.del(e.b, f.id); w.Code != http.StatusNoContent {
				t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
			}
			if g, a := e.userRows(purgeSub); g != 0 || a != 0 {
				t.Errorf("the leaver's own rows left: %d grants, %d assignments", g, a)
			}
			if g, _ := e.userRows(purgeEmail); g != 1 {
				t.Errorf("%d email-subject grants left, want the held address's one kept", g)
			}
			rows := e.purgeRows(e.a, e.b)
			if len(rows) != 1 {
				t.Fatalf("%d purge rows, want 1", len(rows))
			}
			if d := dataOf(t, rows[0]); d["grants_deleted"] != float64(1) || d["email_rows_kept"] != float64(1) {
				t.Errorf("purge row = %v, want 1 grant deleted and 1 email row kept", d)
			}
		})
	}
}
