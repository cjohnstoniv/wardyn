// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	scimCovWsSub    = uuid.MustParse("00000000-0000-4000-8000-0000000000a1")
	scimCovWsEmail  = uuid.MustParse("00000000-0000-4000-8000-0000000000a2")
	scimCovWsOther  = uuid.MustParse("00000000-0000-4000-8000-0000000000a3")
	scimCovWsNobody = uuid.MustParse("00000000-0000-4000-8000-0000000000a4")
)

// scimCovSeedPurgee adds to a leaver the rest of what a purge removes: workspaces under two forms of the
// person, a bystander's and an unowned one, and drive allocations, two of them the person's.
func scimCovSeedPurgee(st *scimCovStore) scimCovLeaver {
	l := scimCovSeedLeaver(st)
	st.workspaces = []types.Workspace{
		{ID: scimCovWsSub, Name: "one", OwnedBy: "sub-pat"},
		{ID: scimCovWsEmail, Name: "two", OwnedBy: "PAT@corp.example"},
		{ID: scimCovWsOther, Name: "three", OwnedBy: "sub-bystander"},
		{ID: scimCovWsNobody, Name: "four"},
	}
	zeta, alpha, other, group := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	drive := func(id uuid.UUID, name string) types.UserDriveListItem {
		return types.UserDriveListItem{UserDrive: types.UserDrive{ID: id, Name: name}}
	}
	st.drives = []types.UserDriveListItem{drive(zeta, "zeta"), drive(alpha, "alpha"), drive(other, "other"), drive(group, "group-drive")}
	st.grants = []types.UserDriveGrant{
		{SubjectType: types.CapabilitySubjectUser, Subject: "sub-pat", DriveID: zeta},
		{SubjectType: types.CapabilitySubjectUser, Subject: "Pat@Corp.example", DriveID: alpha},
		{SubjectType: types.CapabilitySubjectUser, Subject: "pat@corp.example", DriveID: zeta},
		{SubjectType: types.CapabilitySubjectUser, Subject: "sub-bystander", DriveID: other},
		{SubjectType: types.CapabilitySubjectAll, Subject: "sub-pat", DriveID: group},
	}
	st.grantsDeleted, st.assignDeleted = 3, 2
	return l
}

func (e *scimCovEnv) purgeRow() types.AuditEvent {
	e.t.Helper()
	for _, ev := range e.auditRows("person.deprovision") {
		if e.auditData(ev)["kind"] == store.JobKindPurge {
			return ev
		}
	}
	e.t.Fatalf("no person.deprovision row of a purge in %+v", e.auditRows("person.deprovision"))
	return types.AuditEvent{}
}

func (e *scimCovEnv) purgeRows() int {
	n := 0
	for _, ev := range e.auditRows("person.deprovision") {
		if e.auditData(ev)["kind"] == store.JobKindPurge {
			n++
		}
	}
	return n
}

func TestSCIMCovDeletePurgesAndRecordsEveryStep(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), "")
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("DELETE = %d %q, want 204 with no body", w.Code, w.Body.String())
	}
	if !slices.Equal(st.purgeMarks, []bool{false}) {
		t.Errorf("MarkIdentityPurged requireDue = %v, want one unconditional mark", st.purgeMarks)
	}
	ident := st.identity(l.id)
	if ident.PurgedAt == nil || ident.DeactivatedAt == nil || len(st.plans) != 1 {
		t.Errorf("identity %+v plans %d, want a tombstone that was also suspended", ident, len(st.plans))
	}
	if st.runState(l.runSub) != types.RunKilled || !st.tokenRevoked(l.tokSub) {
		t.Error("the purge did not include the suspension's sweeps")
	}

	if want := []scimCovOwnerWrite{{scimCovWsSub, ""}, {scimCovWsEmail, ""}}; !slices.Equal(st.ownerWrites, want) {
		t.Errorf("workspace owner writes = %+v, want the person's two workspaces handed to the operator", st.ownerWrites)
	}
	if st.workspaces[2].OwnedBy != "sub-bystander" {
		t.Errorf("a bystander's workspace owner became %q", st.workspaces[2].OwnedBy)
	}
	rows := e.auditRows("workspace.reassign")
	if len(rows) != 2 || rows[0].Target != scimCovWsSub.String() || e.auditData(rows[0])["from_owner"] != "sub-pat" || e.auditData(rows[0])["reason"] != "scim_purge" ||
		e.auditData(rows[1])["from_owner"] != "PAT@corp.example" {
		t.Errorf("workspace.reassign rows = %+v, want one per workspace naming the old owner and the reason", rows)
	}
	if len(st.deleteRows) != 1 || !slices.Equal(st.deleteRows[0], scimCovTargets) {
		t.Errorf("DeleteUserSubjectRows got %v, want one call over every form", st.deleteRows)
	}

	d := e.auditData(e.purgeRow())
	if d["workspaces_reassigned"] != float64(2) || d["grants_deleted"] != float64(3) || d["assignments_deleted"] != float64(2) ||
		d["credentials_erased"] != float64(0) || d["masks_fenced"] != float64(0) {
		t.Errorf("purge row data = %v, want the counts of the steps", d)
	}
	if drives, _ := d["drives"].([]any); len(drives) != 2 || drives[0] != "alpha" || drives[1] != "zeta" {
		t.Errorf("drives = %v, want the person's drives once each, sorted, and no one else's", d["drives"])
	}
	for _, j := range st.ledger(l.id, store.JobKindPurge) {
		if !j.Done {
			t.Errorf("purge ledger row %s/%s is pending: %q", j.Step, j.Target, j.LastError)
		}
	}
	var erase []string
	for _, j := range st.ledger(l.id, store.JobKindPurge) {
		if j.Step == jobStepErase {
			erase = append(erase, j.Target)
		}
	}
	if !slices.Equal(erase, scimCovTargets[:2]) {
		t.Errorf("erase rows = %v, want the sub and the object-id principal, no bare email", erase)
	}
}

func TestSCIMCovRepeatedDeleteWritesNothingNew(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Users/" + l.id.String()
	e.scim(http.MethodDelete, path, "")
	rows, writes, deletes := len(e.h.audit.snapshot()), len(st.ownerWrites), len(st.deleteRows)

	if w := e.scim(http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
		t.Fatalf("the repeated DELETE = %d %s", w.Code, w.Body.String())
	}
	if len(st.ownerWrites) != writes || len(st.deleteRows) != deletes || len(e.h.audit.snapshot()) != rows {
		t.Errorf("the repeat wrote: owner writes %d->%d, row deletes %d->%d, audit %d->%d",
			writes, len(st.ownerWrites), deletes, len(st.deleteRows), rows, len(e.h.audit.snapshot()))
	}
	if e.purgeRows() != 1 {
		t.Errorf("person.deprovision of a purge written %d times, want once", e.purgeRows())
	}
}

func TestSCIMCovDeleteKeepsWorkspacesWhenAsked(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	e := newSCIMCovEnv(t, st, func(cfg *Config) { cfg.SCIM.KeepWorkspaces = true })

	if w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if len(st.ownerWrites) != 0 || st.workspaces[0].OwnedBy != "sub-pat" || len(e.auditRows("workspace.reassign")) != 0 {
		t.Errorf("owner writes %+v, first workspace owner %q: a keep setting must leave workspaces with the person", st.ownerWrites, st.workspaces[0].OwnedBy)
	}
	if d := e.auditData(e.purgeRow()); d["workspaces_reassigned"] != float64(0) || d["grants_deleted"] != float64(3) {
		t.Errorf("purge row data = %v, want no workspaces reassigned but the grants still deleted", d)
	}
}

func TestSCIMCovDeleteRefusals(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	e := newSCIMCovEnv(t, st)

	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		scimCovWantError(t, e.scim(http.MethodDelete, "/scim/v2/Users/"+id, ""), http.StatusNotFound, "")
	}
	if len(st.purgeMarks) != 0 {
		t.Errorf("an unknown id reached the tombstone: %v", st.purgeMarks)
	}

	st.failNext("MarkIdentityPurged", errSCIMCovBoom, 1)
	scimCovWantRetry(t, e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""), "secret-dsn")
	if st.identity(l.id).PurgedAt != nil || len(st.plans) != 0 || len(st.ownerWrites) != 0 {
		t.Error("a failed mark was followed by the suspension or the purge steps")
	}

	// A suspension that cannot finish keeps the tombstone but touches none of what a purge removes.
	st.failNext("SuspendIdentity", errSCIMCovBoom, 1)
	scimCovWantRetry(t, e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""), "secret-dsn")
	if st.identity(l.id).PurgedAt == nil {
		t.Error("the tombstone must stand once marked")
	}
	if len(st.ownerWrites) != 0 || len(st.deleteRows) != 0 || e.purgeRows() != 0 {
		t.Errorf("purge steps ran before the suspension finished: owner writes %v deletes %v", st.ownerWrites, st.deleteRows)
	}
}

func TestSCIMCovAFailedPurgeStepStaysPendingWhileTheOthersRun(t *testing.T) {
	for _, c := range []struct {
		name, method, step string
		wantWrites         int
		wantDeletes        int
	}{
		{"handing over workspaces", "SetWorkspaceOwner", jobStepWorkspaces, 1, 1},
		{"deleting grants", "DeleteUserSubjectRows", jobStepGrants, 2, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			l := scimCovSeedPurgee(st)
			e := newSCIMCovEnv(t, st)
			st.failNext(c.method, errSCIMCovBoom, 1)
			path := "/scim/v2/Users/" + l.id.String()

			scimCovWantRetry(t, e.scim(http.MethodDelete, path, ""), "secret-dsn")
			var failed store.DeprovisionJob
			for _, j := range st.ledger(l.id, store.JobKindPurge) {
				if !j.Done {
					failed = j
				}
			}
			if failed.Step != c.step || !strings.Contains(failed.LastError, "driver exploded") || failed.Attempts != 1 {
				t.Errorf("pending row = %+v, want %s with the error and one attempt", failed, c.step)
			}
			if len(st.ownerWrites) != c.wantWrites || len(st.deleteRows) != c.wantDeletes {
				t.Errorf("owner writes %d deletes %d, want %d and %d: every step is attempted whether or not another failed",
					len(st.ownerWrites), len(st.deleteRows), c.wantWrites, c.wantDeletes)
			}
			if e.purgeRows() != 0 {
				t.Error("person.deprovision was written with a step pending")
			}

			if w := e.scim(http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
				t.Fatalf("retry = %d %s", w.Code, w.Body.String())
			}
			if e.purgeRows() != 1 || len(st.pendingSteps(l.id, store.JobKindPurge)) != 0 {
				t.Errorf("after the retry: purge rows %d pending %v", e.purgeRows(), st.pendingSteps(l.id, store.JobKindPurge))
			}
			if got := e.auditData(e.purgeRow())["workspaces_reassigned"]; got != float64(2) {
				t.Errorf("workspaces_reassigned = %v, want 2 once the retry handed both over", got)
			}
		})
	}
}

func TestSCIMCovPurgeAuditRowWaitsForItsInputs(t *testing.T) {
	for _, c := range []struct {
		name, method string
	}{
		{"reading the drive allocations", "ListUserDriveGrants"},
		{"reading the drives", "ListUserDrives"},
		{"opening the purge ledger", "EnsureDeprovisionJobs:purge"},
		{"reading the purge ledger", "ListDeprovisionJobs:purge"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			l := scimCovSeedPurgee(st)
			e := newSCIMCovEnv(t, st)
			st.failNext(c.method, errSCIMCovBoom, -1)

			w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), "")
			scimCovWantRetry(t, w, "secret-dsn")
			if e.purgeRows() != 0 {
				t.Error("person.deprovision of a purge was written after a failed read")
			}
			if strings.HasPrefix(c.method, "EnsureDeprovisionJobs") || strings.HasPrefix(c.method, "ListDeprovisionJobs") {
				if len(st.ownerWrites) != 0 || len(st.deleteRows) != 0 {
					t.Error("purge steps ran without a ledger")
				}
			}
		})
	}
}

func TestSCIMCovPurgeAuditWriteIsRetried(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	gate := &scimCovPurgeAudit{}
	e := newSCIMCovEnv(t, st, func(cfg *Config) {
		gate.recRecorder = &recRecorder{}
		cfg.Audit = gate
	})
	gate.setFail(true)
	path := "/scim/v2/Users/" + l.id.String()

	scimCovWantRetry(t, e.scim(http.MethodDelete, path, ""), "audit sink")
	left := st.pendingSteps(l.id, store.JobKindPurge)
	if !slices.Equal(left, []string{store.JobStepAuditDeprovision + "/"}) {
		t.Errorf("pending = %v, want only the audit row", left)
	}
	var lastErr string
	for _, j := range st.ledger(l.id, store.JobKindPurge) {
		if j.Step == store.JobStepAuditDeprovision {
			lastErr = j.LastError
		}
	}
	if lastErr != "audit sink unavailable" {
		t.Errorf("audit row last_error = %q, want the sink's error", lastErr)
	}

	gate.setFail(false)
	if w := e.scim(http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
		t.Fatalf("retry = %d %s", w.Code, w.Body.String())
	}
	var written int
	for _, ev := range gate.snapshot() {
		if ev.Action == "person.deprovision" && strings.Contains(string(ev.Data), `"kind":"purge"`) {
			written++
		}
	}
	if written != 1 || len(st.pendingSteps(l.id, store.JobKindPurge)) != 0 {
		t.Errorf("purge rows %d pending %v, want one row and nothing pending", written, st.pendingSteps(l.id, store.JobKindPurge))
	}
}

func TestSCIMCovReassignWorkspaces(t *testing.T) {
	ctx := withActor(context.Background(), types.ActorSystem, scimActor)
	t.Run("a failed listing writes nothing", func(t *testing.T) {
		st := newSCIMCovStore()
		scimCovSeedPurgee(st)
		e := newSCIMCovEnv(t, st)
		st.failNext("ListWorkspaces", errSCIMCovBoom, 1)
		if _, err := e.srv.reassignWorkspaces(ctx, []string{"sub-pat"}); !errors.Is(err, errSCIMCovBoom) || len(st.ownerWrites) != 0 {
			t.Errorf("err = %v owner writes %v", err, st.ownerWrites)
		}
	})
	t.Run("a failed write stops after the ones already done", func(t *testing.T) {
		st := newSCIMCovStore()
		scimCovSeedPurgee(st)
		e := newSCIMCovEnv(t, st)
		st.failNext("SetWorkspaceOwner", errSCIMCovBoom, -1)
		if _, err := e.srv.reassignWorkspaces(ctx, scimCovTargets); !errors.Is(err, errSCIMCovBoom) {
			t.Errorf("err = %v, want the store's error", err)
		}
		if len(st.ownerWrites) != 1 || len(e.auditRows("workspace.reassign")) != 0 {
			t.Errorf("owner writes %v, reassign rows %d: no row may claim a hand-over the store refused", st.ownerWrites, len(e.auditRows("workspace.reassign")))
		}
	})
	t.Run("an owner matches in any case and an unowned workspace never", func(t *testing.T) {
		st := newSCIMCovStore()
		scimCovSeedPurgee(st)
		e := newSCIMCovEnv(t, st)
		got, err := e.srv.reassignWorkspaces(ctx, []string{"pat@corp.example"})
		if err != nil || got["workspaces_reassigned"] != 1 || len(st.ownerWrites) != 1 || st.ownerWrites[0].ID != scimCovWsEmail {
			t.Errorf("got %v %v owner writes %v, want only the workspace owned under the email", got, err, st.ownerWrites)
		}
	})
}

func TestSCIMCovPersonDrivesListsOnlyTheSubjectsUserGrants(t *testing.T) {
	st := newSCIMCovStore()
	scimCovSeedPurgee(st)
	e := newSCIMCovEnv(t, st)
	ctx := context.Background()

	got, err := e.srv.personDrives(ctx, []string{"SUB-PAT"})
	if err != nil || !slices.Equal(got, []string{"zeta"}) {
		t.Errorf("personDrives(SUB-PAT) = %v %v, want [zeta]: user grants only, matched in any case", got, err)
	}
	if got, err := e.srv.personDrives(ctx, []string{"nobody"}); err != nil || got == nil || len(got) != 0 {
		t.Errorf("personDrives(nobody) = %#v %v, want an empty non-nil list", got, err)
	}
	st.grants = append(st.grants, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: "sub-pat", DriveID: uuid.New()})
	if got, _ := e.srv.personDrives(ctx, []string{"sub-pat"}); !slices.Equal(got, []string{"zeta"}) {
		t.Errorf("a grant of a drive that no longer exists listed %v", got)
	}
}

func TestSCIMCovEraseForPurgeSkipsScopesThatAreNotConfigured(t *testing.T) {
	st := newSCIMCovStore()
	e := newSCIMCovEnv(t, st)
	detail, err := e.srv.eraseForPurge(context.Background(), "sub-pat")
	if err != nil || detail == nil || len(detail) != 0 {
		t.Errorf("eraseForPurge = %#v %v, want an empty detail and no error when no backing is configured", detail, err)
	}
}

func scimCovSweepEnv(t *testing.T, st *scimCovStore) *scimCovEnv {
	return newSCIMCovEnv(t, st, func(cfg *Config) { cfg.SCIM.PurgeAfter = 24 * time.Hour })
}

func scimCovDeactivated(st *scimCovStore, principal string, due bool) store.PrincipalIdentity {
	gone := scimCovNow.Add(-48 * time.Hour)
	after := scimCovNow.Add(-time.Hour)
	if !due {
		after = scimCovNow.Add(time.Hour)
	}
	return st.addIdentity(store.PrincipalIdentity{Principal: principal, Issuer: scimCovIssuer, DeactivatedAt: &gone, PurgeAfter: &after})
}

func TestSCIMCovSweeperPurgesWhatIsDueUnderItsOwnSlot(t *testing.T) {
	st := newSCIMCovStore()
	due := scimCovDeactivated(st, "sub-due", true)
	notDue := scimCovDeactivated(st, "sub-later", false)
	e := scimCovSweepEnv(t, st)

	if err := e.srv.SweepSCIMPurge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.identity(due.ID).PurgedAt == nil || st.identity(notDue.ID).PurgedAt != nil {
		t.Errorf("due purged %t, not-due purged %t", st.identity(due.ID).PurgedAt != nil, st.identity(notDue.ID).PurgedAt != nil)
	}
	if !slices.Equal(st.purgeMarks, []bool{true}) {
		t.Errorf("MarkIdentityPurged requireDue = %v, want the sweeper's conditional mark", st.purgeMarks)
	}
	rows := e.auditRows("scim.user.deactivate")
	if len(rows) != 1 || e.auditData(rows[0])["slot"] != scimSweeperSlot || rows[0].Target != due.ID.String() {
		t.Errorf("scim.user.deactivate rows = %+v, want one naming the sweeper", rows)
	}
	if e.purgeRows() != 1 {
		t.Errorf("purge rows = %d, want one", e.purgeRows())
	}
	if len(st.pendingArgs) != 2 || st.pendingArgs[0] != scimResumeIdle || st.pendingArgs[1] != time.Duration(scimSweepBatch) {
		t.Errorf("PendingLeavers args = %v, want the resume idle time and the batch cap", st.pendingArgs)
	}
}

func TestSCIMCovSweeperSkipsACandidateReactivatedFirst(t *testing.T) {
	st := newSCIMCovStore()
	cand := scimCovDeactivated(st, "sub-back", true)
	st.afterDue = func() {
		st.mu.Lock()
		st.idents[cand.ID].DeactivatedAt, st.idents[cand.ID].PurgeAfter = nil, nil
		st.mu.Unlock()
	}
	e := scimCovSweepEnv(t, st)

	if err := e.srv.SweepSCIMPurge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.identity(cand.ID).PurgedAt != nil || len(st.plans) != 0 || len(e.h.audit.snapshot()) != 0 {
		t.Errorf("a person reactivated before the re-check was purged: %+v plans %d audit %d", st.identity(cand.ID), len(st.plans), len(e.h.audit.snapshot()))
	}
}

func TestSCIMCovSweeperDoesNothingWithoutSCIMOrTheSCIMStore(t *testing.T) {
	st := newSCIMCovStore()
	scimCovDeactivated(st, "sub-due", true)
	off := newSCIMCovEnv(t, st, func(cfg *Config) { cfg.SCIM = nil })
	if err := off.srv.SweepSCIMPurge(context.Background()); err != nil || len(st.calls) != 0 {
		t.Errorf("without SCIM: err %v, store calls %v, want none", err, st.calls)
	}
	plain := newSCIMCovEnv(t, rbacStore{})
	if err := plain.srv.SweepSCIMPurge(context.Background()); err != nil || len(plain.h.audit.snapshot()) != 0 {
		t.Errorf("without the SCIM store: err %v audit %v", err, plain.h.audit.snapshot())
	}
}

func TestSCIMCovSweeperAutoPurgeOffStillResumesPendingLeavers(t *testing.T) {
	st := newSCIMCovStore()
	due := scimCovDeactivated(st, "sub-due", true)
	e := newSCIMCovEnv(t, st)

	if err := e.srv.SweepSCIMPurge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.callCount("PurgeDueIdentities") != 0 || st.identity(due.ID).PurgedAt != nil {
		t.Error("a zero purge delay must never purge")
	}
	if st.callCount("PendingLeavers") != 1 {
		t.Errorf("PendingLeavers ran %d times, want once", st.callCount("PendingLeavers"))
	}
}

func TestSCIMCovSweeperContinuesPastAFailedIdentity(t *testing.T) {
	st := newSCIMCovStore()
	first := scimCovDeactivated(st, "sub-first", true)
	second := scimCovDeactivated(st, "sub-second", true)
	st.failNext("MarkIdentityPurged", errSCIMCovBoom, 1)
	e := scimCovSweepEnv(t, st)

	err := e.srv.SweepSCIMPurge(context.Background())
	if !errors.Is(err, errSCIMCovBoom) {
		t.Fatalf("err = %v, want the failure reported", err)
	}
	if st.identity(first.ID).PurgedAt != nil || st.identity(second.ID).PurgedAt == nil {
		t.Errorf("first purged %t second purged %t, want only the second", st.identity(first.ID).PurgedAt != nil, st.identity(second.ID).PurgedAt != nil)
	}
}

func TestSCIMCovSweeperReportsListFailuresAndKeepsGoing(t *testing.T) {
	st := newSCIMCovStore()
	due := scimCovDeactivated(st, "sub-due", true)
	e := scimCovSweepEnv(t, st)
	st.failNext("PurgeDueIdentities", errSCIMCovBoom, 1)
	st.failNext("PendingLeavers", errors.New("pending list unavailable"), 1)

	err := e.srv.SweepSCIMPurge(context.Background())
	if !errors.Is(err, errSCIMCovBoom) || err == nil || !strings.Contains(err.Error(), "pending list unavailable") {
		t.Errorf("err = %v, want both list failures joined", err)
	}
	if st.identity(due.ID).PurgedAt != nil {
		t.Error("a failed candidate listing purged someone")
	}
}

func TestSCIMCovSweeperResumesSuspensionsAndPurges(t *testing.T) {
	st := newSCIMCovStore()
	// A suspension its provider stopped retrying: deactivated, the run kill still pending.
	stuck := scimCovDeactivated(st, "sub-stuck", false)
	run := st.addRun("sub-stuck", types.RunRunning, "sbx")
	st.jobs = append(st.jobs, &store.DeprovisionJob{IdentityID: stuck.ID, Kind: store.JobKindSuspend, Step: store.JobStepCutoff, Done: true})
	// A purge begun and not finished: the tombstone is set and only the audit step is on the ledger.
	half := scimCovDeactivated(st, "sub-half", false)
	st.mu.Lock()
	t0 := scimCovNow
	st.idents[half.ID].PurgedAt = &t0
	st.jobs = append(st.jobs,
		&store.DeprovisionJob{IdentityID: half.ID, Kind: store.JobKindSuspend, Step: store.JobStepCutoff, Done: true},
		&store.DeprovisionJob{IdentityID: half.ID, Kind: store.JobKindPurge, Step: store.JobStepAuditDeprovision})
	st.mu.Unlock()
	st.workspaces = []types.Workspace{{ID: scimCovWsSub, Name: "w", OwnedBy: "sub-half"}}
	st.pending = []store.PendingLeaver{{ID: stuck.ID}, {ID: half.ID, Purged: true}}
	e := newSCIMCovEnv(t, st)

	if err := e.srv.SweepSCIMPurge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.runState(run) != types.RunKilled {
		t.Errorf("the stuck suspension's run is %s, want KILLED", st.runState(run))
	}
	if left := st.pendingSteps(stuck.ID, store.JobKindSuspend); len(left) != 0 {
		t.Errorf("stuck suspension still pending: %v", left)
	}
	if left := st.pendingSteps(half.ID, store.JobKindPurge); len(left) != 0 {
		t.Errorf("half purge still pending: %v", left)
	}
	if len(st.ownerWrites) != 1 || st.ownerWrites[0] != (scimCovOwnerWrite{scimCovWsSub, ""}) {
		t.Errorf("owner writes = %+v, want the half-purged person's workspace handed over", st.ownerWrites)
	}
	if len(st.purgeMarks) != 0 {
		t.Errorf("resuming marked a tombstone again: %v", st.purgeMarks)
	}
	for _, ev := range e.auditRows("scim.user.deactivate") {
		if e.auditData(ev)["slot"] != scimSweeperSlot {
			t.Errorf("a resumed suspension named slot %v, want the sweeper", e.auditData(ev)["slot"])
		}
	}
}

// The purge reads the identity, its forms and the purge ledger again after the suspension; a read that fails
// there stops the purge steps and leaves the tombstone.
func TestSCIMCovPurgeStopsWhenALaterReadFails(t *testing.T) {
	for _, c := range []struct {
		name, method string
		skip         int
		wantWrites   int
	}{
		{"reading the identity again", "GetIdentity", 1, 0},
		{"reading the forms again", "IdentityAliasValues", 1, 0},
		{"reading the ledger for the audit row", "ListDeprovisionJobs:purge", 1, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			l := scimCovSeedPurgee(st)
			e := newSCIMCovEnv(t, st)
			st.failAfter(c.method, errSCIMCovBoom, c.skip)

			purged, err := e.srv.purgeIdentity(context.Background(), st, l.id, scimSweeperSlot, false)
			if !errors.Is(err, errSCIMCovBoom) || !purged {
				t.Fatalf("purged %t err %v, want the read's error", purged, err)
			}
			if len(st.ownerWrites) != c.wantWrites || e.purgeRows() != 0 || st.identity(l.id).PurgedAt == nil {
				t.Errorf("owner writes %d purge rows %d, want %d writes and no audit row, with the tombstone kept", len(st.ownerWrites), e.purgeRows(), c.wantWrites)
			}
		})
	}
}

func TestSCIMCovPurgeErasesStoredCredentialsOfEveryPrincipalForm(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	sec := &scimCovSecrets{memSecrets: &memSecrets{m: map[string][]byte{"operator-key": []byte("operator-value-0000000")}}}
	ctx := context.Background()
	for owner, names := range map[string][]string{"sub-pat": {"a", "b"}, scimCovObjectForm: {"c"}, "sub-bystander": {"d"}} {
		for _, n := range names {
			if err := sec.For(owner).Put(ctx, n, []byte("seeded-credential-value-0000")); err != nil {
				t.Fatal(err)
			}
		}
	}
	e := newSCIMCovEnv(t, st, func(cfg *Config) { cfg.Secrets = sec })

	if w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	for _, owner := range []string{"sub-pat", scimCovObjectForm} {
		if got, _ := sec.For(owner).List(ctx); len(got) != 0 {
			t.Errorf("%s still holds %v", owner, got)
		}
	}
	if got, _ := sec.For("sub-bystander").List(ctx); !slices.Equal(got, []string{"d"}) {
		t.Errorf("a bystander holds %v, want the credential untouched", got)
	}
	if got, _ := sec.For("").List(ctx); !slices.Equal(got, []string{"operator-key"}) {
		t.Errorf("the operator namespace holds %v, want it untouched", got)
	}
	if got := e.auditData(e.purgeRow())["credentials_erased"]; got != float64(3) {
		t.Errorf("credentials_erased = %v, want the 3 credentials of the two forms", got)
	}
}

func TestSCIMCovAFailedCredentialEraseStaysPendingUntilRetried(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	sec := &scimCovSecrets{memSecrets: &memSecrets{m: map[string][]byte{}}}
	ctx := context.Background()
	if err := sec.For("sub-pat").Put(ctx, "a", []byte("seeded-credential-value-0000")); err != nil {
		t.Fatal(err)
	}
	e := newSCIMCovEnv(t, st, func(cfg *Config) { cfg.Secrets = sec })
	sec.arm("sub-pat", errors.New("credential store unavailable"))
	path := "/scim/v2/Users/" + l.id.String()

	scimCovWantRetry(t, e.scim(http.MethodDelete, path, ""), "credential store")
	left := st.pendingSteps(l.id, store.JobKindPurge)
	if !slices.Equal(left, []string{store.JobStepAuditDeprovision + "/", jobStepErase + "/sub-pat"}) {
		t.Errorf("pending = %v, want the erase of that one form and the audit row", left)
	}
	if len(st.ownerWrites) != 2 || len(st.deleteRows) != 1 {
		t.Errorf("owner writes %d deletes %d: the other steps must still run", len(st.ownerWrites), len(st.deleteRows))
	}

	sec.arm("", nil)
	if w := e.scim(http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
		t.Fatalf("retry = %d %s", w.Code, w.Body.String())
	}
	if got, _ := sec.For("sub-pat").List(ctx); len(got) != 0 || len(st.pendingSteps(l.id, store.JobKindPurge)) != 0 {
		t.Errorf("after the retry the form still holds %v, pending %v", got, st.pendingSteps(l.id, store.JobKindPurge))
	}
	if got := e.auditData(e.purgeRow())["credentials_erased"]; got != float64(1) {
		t.Errorf("credentials_erased = %v, want 1", got)
	}
}
