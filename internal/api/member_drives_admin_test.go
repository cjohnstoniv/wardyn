// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// seedMemberDrive stores owner's docker drive and returns it.
func seedMemberDrive(st *memberDriveStore, owner, name string) types.UserDrive {
	d := types.UserDrive{
		ID: uuid.New(), Name: name, Backend: types.DriveBackendDockerVolume, HomeTemplate: types.HomeTemplateHash,
		ObjectScheme: types.DriveObjectSchemeID, Writable: true, Reclaim: types.DriveReclaimDelete, OwnerSubject: owner,
	}
	st.drives[d.ID] = d
	return d
}

func deleteMyDrive(srv *Server, sub string, id string) *httptest.ResponseRecorder {
	return memberCall(personCtx(sub), srv.handleDeleteMyDrive, http.MethodDelete, "/api/v1/me/drives/"+id, "", map[string]string{"id": id})
}

// TestMemberDriveForeignIDUniform404: Bob deleting Alice's drive, an admin
// drive or an id that names nothing gets the same status and body, and the
// substrate is never called.
func TestMemberDriveForeignIDUniform404(t *testing.T) {
	st := newMemberDriveStore()
	alices := seedMemberDrive(st, aliceSub, "Notes")
	admin := types.UserDrive{ID: uuid.New(), Name: "Corp", Backend: types.DriveBackendDockerVolume, HomeTemplate: types.HomeTemplateHash}
	st.drives[admin.ID] = admin
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, _ := memberDriveServer(st, rn, "docker")
	norm := func(id uuid.UUID, body []byte) []byte {
		return bytes.ReplaceAll(body, []byte(id.String()), []byte("ID"))
	}
	var first []byte
	for _, id := range []uuid.UUID{alices.ID, admin.ID, uuid.New()} {
		w := deleteMyDrive(srv, bobSub, id.String())
		wantReason(t, w, http.StatusNotFound, string(authz.ReasonDriveNotFound))
		if b := norm(id, w.Body.Bytes()); first == nil {
			first = b
		} else if !bytes.Equal(b, first) {
			t.Errorf("body for %s = %s, want the same bytes as %s", id, b, first)
		}
	}
	if len(rn.calls) != 0 {
		t.Errorf("substrate calls = %d, want 0", len(rn.calls))
	}
	if _, ok := st.drives[alices.ID]; !ok || st.drives[alices.ID].DeleteStartedAt != nil {
		t.Errorf("Alice's drive was touched by Bob's delete")
	}
}

// TestMemberDriveDeleteDestroysThenRemoves: the substrate is asked to destroy
// the object the owner's run mounted; deleted or already_absent removes the
// row; in use keeps it and clears the tombstone; a failure keeps it. Every
// attempt is one audit row with its outcome.
func TestMemberDriveDeleteDestroysThenRemoves(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome runner.DriveReclaimOutcome
		err     error
		status  int
		reason  string
		kept    bool
		held    bool // the claim stays: the answer did not say nothing was touched
		audit   string
	}{
		{"deleted", runner.DriveReclaimDeleted, nil, http.StatusNoContent, "", false, false, "deleted"},
		{"already absent", runner.DriveReclaimAlreadyAbsent, nil, http.StatusNoContent, "", false, false, "already_absent"},
		{"in use", "", runner.ErrDriveInUse, http.StatusConflict, reasonMemberDriveInUse, true, false, "refused"},
		{"not this drive's, said untouched", "", runner.Untouched(runner.ErrDriveNotReclaimable), http.StatusConflict, reasonMemberDriveInUse, true, false, "refused"},
		{"not this drive's, not said untouched", "", runner.ErrDriveNotReclaimable, http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, true, true, "failed"},
		{"claim already terminating", "", fmt.Errorf("k8s: drive: claim is already Terminating: %w", runner.ErrDriveNotReclaimable), http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, true, true, "failed"},
		{"substrate failed", "", errors.New("daemon down"), http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, true, true, "failed"},
		{"empty reply", "", nil, http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, true, true, "failed"},
		{"unrecognised reply", "purged", nil, http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, true, true, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemberDriveStore()
			d := seedMemberDrive(st, aliceSub, "Notes")
			rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: tc.outcome, err: tc.err}}
			srv, rec := memberDriveServer(st, rn, "docker")
			w := deleteMyDrive(srv, aliceSub, d.ID.String())
			if tc.reason == "" && w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			} else if tc.reason != "" {
				body := wantReason(t, w, tc.status, tc.reason)
				if tc.reason == reasonMemberDriveDeleteFailed && body.Error != memberDriveDeleteFailedMsg() {
					t.Errorf("message = %q, want %q", body.Error, memberDriveDeleteFailedMsg())
				}
			}
			home, _ := types.DriveHomeName(d, aliceSub, "")
			if len(rn.calls) != 1 || rn.calls[0].ObjectName != types.DriveObjectName(d, home) ||
				rn.calls[0].SubjectHash != types.DriveSubjectHash(aliceSub) || rn.calls[0].DriveID != d.ID {
				t.Fatalf("substrate calls = %+v, want one for the owner's object", rn.calls)
			}
			got, kept := st.drives[d.ID]
			if kept != tc.kept || kept && (got.DeleteStartedAt != nil) != tc.held {
				t.Errorf("row kept = %v (tombstone %v), want kept = %v with the claim held = %v", kept, got.DeleteStartedAt, tc.kept, tc.held)
			}
			if tc.held && kept && got.DeleteIssues != 1 {
				t.Errorf("held row's issue count = %d, want the one issued call (1)", got.DeleteIssues)
			}
			if n := countAction(rec, "drive.member.delete"); n != 1 {
				t.Fatalf("drive.member.delete rows = %d, want 1", n)
			}
			data := driveAuditData(t, rec, "drive.member.delete")
			if data["outcome"] != tc.audit || data["by"] != "owner" || data["owner"] != aliceSub || data["object"] != rn.calls[0].ObjectName {
				t.Errorf("audit = %v", data)
			}
		})
	}
	t.Run("busy", func(t *testing.T) {
		st := newMemberDriveStore()
		d := seedMemberDrive(st, aliceSub, "Notes")
		at := st.now.Add(-10 * time.Second)
		d.DeleteStartedAt = &at
		st.drives[d.ID] = d
		rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
		srv, _ := memberDriveServer(st, rn, "docker")
		wantReason(t, deleteMyDrive(srv, aliceSub, d.ID.String()), http.StatusConflict, reasonMemberDriveBusy)
		if len(rn.calls) != 0 {
			t.Errorf("a busy drive reached the substrate")
		}
	})
	t.Run("substrate cannot delete here", func(t *testing.T) {
		st := newMemberDriveStore()
		d := seedMemberDrive(st, aliceSub, "Notes")
		rn := &memberDriveRunner{lifecycleErr: runner.ErrDriveLifecycleUnavailable}
		srv, rec := memberDriveServer(st, rn, "docker")
		wantReason(t, deleteMyDrive(srv, aliceSub, d.ID.String()), http.StatusUnprocessableEntity, reasonMemberDriveDeleteUnavailable)
		if _, kept := st.drives[d.ID]; !kept || len(rn.calls) != 0 || driveAuditData(t, rec, "drive.member.delete")["outcome"] != "failed" {
			t.Errorf("kept = %v, calls = %d", kept, len(rn.calls))
		}
	})
}

// TestAdminRoutesRefuseMemberDrives: every admin drive, grant and reclaim route
// refuses a person's drive with 409 before it writes or reaches the substrate,
// and GET /drives is the same with and without people's drives.
func TestAdminRoutesRefuseMemberDrives(t *testing.T) {
	st := newMemberDriveStore()
	admin := types.UserDrive{ID: uuid.New(), Name: "Corp", Backend: types.DriveBackendDockerVolume, HomeTemplate: types.HomeTemplateHash}
	st.drives[admin.ID] = admin
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, _ := memberDriveServer(st, rn, "docker")
	adminCtx := operatorCtx("sub-admin", "admin@corp.example", "admin")
	list := func() []byte {
		w := memberCall(adminCtx, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /drives: %d %s", w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	before := list()
	d := seedMemberDrive(st, aliceSub, "Notes")
	if after := list(); !bytes.Equal(before, after) {
		t.Errorf("GET /drives changed with a person's drive present:\n%s\n%s", before, after)
	}
	id := d.ID.String()
	for name, w := range map[string]*httptest.ResponseRecorder{
		"PUT /drives/{id}": memberCall(adminCtx, srv.handleUpdateUserDrive, http.MethodPut, "/x",
			`{"name":"Hijack","backend":"docker_volume","home_template":"hash"}`, map[string]string{"id": id}),
		"DELETE /drives/{id}": memberCall(adminCtx, srv.handleDeleteUserDrive, http.MethodDelete, "/x", "", map[string]string{"id": id}),
		"POST /drives/grants": memberCall(adminCtx, srv.handleUpsertUserDriveGrant, http.MethodPost, "/x",
			`{"subject_type":"group","subject":"eng","drive_id":"`+id+`"}`, nil),
		"POST /drives/{id}/reclaim": memberCall(adminCtx, srv.handleReclaimUserDrive, http.MethodPost, "/x",
			`{"subject_type":"user","subject":"`+aliceSub+`"}`, map[string]string{"id": id}),
	} {
		t.Run(name, func(t *testing.T) { wantReason(t, w, http.StatusConflict, reasonUserDriveMemberOwned) })
	}
	if got := st.drives[d.ID]; got.Name != "Notes" || got.OwnerSubject != aliceSub {
		t.Errorf("the person's drive changed: %+v", got)
	}
	if len(st.grants) != 0 {
		t.Errorf("grants changed: %v", st.grants)
	}
	if len(rn.calls) != 0 {
		t.Errorf("reclaim reached the substrate %d times", len(rn.calls))
	}
}

// TestMemberDriveDeleteRefusesBeforeTheTombstoneWhileARunStarts: a run of the owner's in STARTING may
// attach the drive, so the owner's delete answers in use before writing the tombstone that run would
// meet as drive_not_found, and never reaches the substrate. Another person's STARTING run does not
// hold it.
func TestMemberDriveDeleteRefusesBeforeTheTombstoneWhileARunStarts(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	st.starting = map[string]bool{aliceSub: true}
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, rec := memberDriveServer(st, rn, "docker")
	wantReason(t, deleteMyDrive(srv, aliceSub, d.ID.String()), http.StatusConflict, reasonMemberDriveInUse)
	if got, kept := st.drives[d.ID]; !kept || got.DeleteStartedAt != nil || st.begins != 0 || len(rn.calls) != 0 {
		t.Fatalf("kept = %v (tombstone %v), tombstone writes = %d, substrate calls = %d; want kept, never tombstoned, no call",
			kept, got.DeleteStartedAt, st.begins, len(rn.calls))
	}
	if data := driveAuditData(t, rec, "drive.member.delete"); data["outcome"] != "refused" || data["by"] != "owner" {
		t.Errorf("audit = %v, want outcome refused by the owner", data)
	}
	st.starting = map[string]bool{bobSub: true}
	if w := deleteMyDrive(srv, aliceSub, d.ID.String()); w.Code != http.StatusNoContent || len(rn.calls) != 1 {
		t.Fatalf("delete beside another person's starting run = %d (calls %d), want 204 after one reclaim: %s", w.Code, len(rn.calls), w.Body.String())
	}
}

// TestMemberDriveAdminDeleteRefusesBeforeTheTombstoneWhileARunStarts: the
// admin route reads the drive's owner first, so a run of the owner's in
// STARTING keeps the drive with no tombstone written and no substrate call.
func TestMemberDriveAdminDeleteRefusesBeforeTheTombstoneWhileARunStarts(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	st.starting = map[string]bool{aliceSub: true}
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, rec := memberDriveServer(st, rn, "docker")
	adminCtx := operatorCtx("sub-admin", "admin@corp.example", "admin")
	w := memberCall(adminCtx, srv.handleDeleteMemberDriveAdmin, http.MethodDelete, "/x", "", map[string]string{"id": d.ID.String()})
	wantReason(t, w, http.StatusConflict, reasonUserDriveReclaimConflict)
	if got, kept := st.drives[d.ID]; !kept || got.DeleteStartedAt != nil || st.begins != 0 || len(rn.calls) != 0 {
		t.Fatalf("kept = %v (tombstone %v), tombstone writes = %d, substrate calls = %d; want kept, never tombstoned, no call",
			kept, got.DeleteStartedAt, st.begins, len(rn.calls))
	}
	if data := driveAuditData(t, rec, "drive.member.delete"); data["outcome"] != "refused" || data["by"] != "admin" {
		t.Errorf("audit = %v, want outcome refused by the admin", data)
	}
}

// TestMemberDriveDeleteWaitsOutAStartingRun: past the tombstone the starting
// read runs again, so a run that started between the first read and the
// tombstone still keeps the drive and clears the tombstone.
func TestMemberDriveDeleteWaitsOutAStartingRun(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, rec := memberDriveServer(st, rn, "docker")
	ctx := withActor(context.Background(), types.ActorHuman, "sub-admin")
	tomb, started, err := st.BeginMemberDriveDelete(ctx, aliceSub, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	st.starting = map[string]bool{aliceSub: true} // the run started after the first read
	if res := srv.deleteMemberDrive(ctx, st, tomb, started, memberDriveByAdmin); res.reason != reasonMemberDriveInUse {
		t.Fatalf("deletion = %+v, want refused in use", res)
	}
	if got, kept := st.drives[d.ID]; !kept || got.DeleteStartedAt != nil || len(rn.calls) != 0 {
		t.Fatalf("kept = %v (tombstone %v), substrate calls = %d; want kept, tombstone cleared, no call", kept, got.DeleteStartedAt, len(rn.calls))
	}
	if data := driveAuditData(t, rec, "drive.member.delete"); data["outcome"] != "refused" {
		t.Errorf("audit = %v, want outcome refused", data)
	}
}

// TestAdminListAndDeleteMemberDrives: the super admin lists every person's
// drive with its owner and object, and deletes one with by=admin.
func TestAdminListAndDeleteMemberDrives(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	seedMemberDrive(st, bobSub, "Logs")
	mac := uuid.New()
	disk := seedMemberDrive(st, bobSub, "Disk")
	disk.Backend, disk.HomeRunnerID = types.DriveBackendMacDisk, &mac
	st.drives[disk.ID] = disk
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, rec := memberDriveServer(st, rn, "docker")
	adminCtx := operatorCtx("sub-admin", "admin@corp.example", "admin")
	w := memberCall(adminCtx, srv.handleListMemberDrives, http.MethodGet, "/api/v1/drives/member", "", nil)
	var list client.MemberDriveList
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Total != 3 || len(list.Drives) != 3 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	// The Mac a disk drive is kept on rides with its row, and only its row.
	for _, it := range list.Drives {
		switch {
		case it.ID == disk.ID && (it.HomeRunnerID == nil || *it.HomeRunnerID != mac || it.Backend != types.DriveBackendMacDisk):
			t.Errorf("mac_disk row = %+v, want home_runner_id %s", it, mac)
		case it.ID != disk.ID && it.HomeRunnerID != nil:
			t.Errorf("row %s carries home_runner_id %s on backend %s", it.Name, it.HomeRunnerID, it.Backend)
		}
	}
	w = memberCall(adminCtx, srv.handleDeleteMemberDriveAdmin, http.MethodDelete, "/x", "", map[string]string{"id": d.ID.String()})
	var res client.MemberDriveDeleteResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Outcome != "deleted" || res.Owner != aliceSub {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, ok := st.drives[d.ID]; ok {
		t.Errorf("row kept after a deleted outcome")
	}
	if data := driveAuditData(t, rec, "drive.member.delete"); data["by"] != "admin" || data["owner"] != aliceSub {
		t.Errorf("audit = %v", data)
	}
	rn.err = runner.ErrDriveInUse
	other := seedMemberDrive(st, bobSub, "Busy")
	w = memberCall(adminCtx, srv.handleDeleteMemberDriveAdmin, http.MethodDelete, "/x", "", map[string]string{"id": other.ID.String()})
	wantReason(t, w, http.StatusConflict, reasonUserDriveReclaimConflict)
}

// sequencedRunner answers its nth reclaim with replies[n] (the last repeats).
type sequencedRunner struct {
	memberDriveRunner
	replies []reclaimReply
}

type reclaimReply struct {
	outcome runner.DriveReclaimOutcome
	err     error
}

func (r *sequencedRunner) ReclaimDrive(_ context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	r.calls = append(r.calls, m)
	rep := r.replies[min(len(r.calls), len(r.replies))-1]
	return rep.outcome, rep.err
}

// TestMemberDriveDeleteHoldsWhatItCannotConfirm (design test 2: rows 2, 4, 5 and the allowlist): an answer
// that is not an affirmative deleted or already_absent, and every error that is not a typed refusal, leaves the
// row held at the issued count (deleting, so hidden) with a 503 and a failed audit row that never says nothing was
// removed. The owner's immediate retry is busy and makes no call; a retry after the claim has aged gets the
// substrate's later answer and the row goes. Through the owner's route and the admin's.
func TestMemberDriveDeleteHoldsWhatItCannotConfirm(t *testing.T) {
	adminCtx := operatorCtx("sub-admin", "admin@corp.example", "admin")
	routes := []struct {
		by         string
		call       func(srv *Server, id string) *httptest.ResponseRecorder
		retryCode  int
		busyReason string
	}{
		{memberDriveByOwner, func(srv *Server, id string) *httptest.ResponseRecorder { return deleteMyDrive(srv, aliceSub, id) }, http.StatusNoContent, reasonMemberDriveBusy},
		{memberDriveByAdmin, func(srv *Server, id string) *httptest.ResponseRecorder {
			return memberCall(adminCtx, srv.handleDeleteMemberDriveAdmin, http.MethodDelete, "/x", "", map[string]string{"id": id})
		}, http.StatusOK, reasonMemberDriveBusy},
	}
	for _, rt := range routes {
		for _, tc := range []struct {
			name  string
			first reclaimReply
			retry reclaimReply
			want  string // the retry's audit outcome
		}{
			{"completed after it returned an error", reclaimReply{err: errors.New("context deadline exceeded")}, reclaimReply{outcome: runner.DriveReclaimAlreadyAbsent}, "already_absent"},
			{"partial remove", reclaimReply{err: errors.New("removed the volume, left its lock")}, reclaimReply{outcome: runner.DriveReclaimDeleted}, "deleted"},
			{"claim terminating", reclaimReply{err: fmt.Errorf("k8s: drive: claim is already Terminating: %w", runner.ErrDriveNotReclaimable)}, reclaimReply{outcome: runner.DriveReclaimAlreadyAbsent}, "already_absent"},
			{"empty reply", reclaimReply{}, reclaimReply{outcome: runner.DriveReclaimDeleted}, "deleted"},
			{"unrecognised reply", reclaimReply{outcome: "purged"}, reclaimReply{outcome: runner.DriveReclaimDeleted}, "deleted"},
		} {
			t.Run(rt.by+"/"+tc.name, func(t *testing.T) {
				st := newMemberDriveStore()
				d := seedMemberDrive(st, aliceSub, "Notes")
				rn := &sequencedRunner{replies: []reclaimReply{tc.first, tc.retry}}
				srv, rec := memberDriveServer(st, rn, "docker")
				id := d.ID.String()

				body := wantReason(t, rt.call(srv, id), http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed)
				if body.Error != memberDriveDeleteFailedMsg() || strings.Contains(body.Error, "Nothing was removed") {
					t.Errorf("message = %q, want the held-while-pending sentence and no promise the files survived", body.Error)
				}
				got, kept := st.drives[d.ID]
				if !kept || !got.Deleting() || got.DeleteIssues != 1 {
					t.Fatalf("row kept = %v, deleting = %v, issues = %d; want held at the one issued call", kept, got.Deleting(), got.DeleteIssues)
				}
				if data := driveAuditData(t, rec, "drive.member.delete"); data["outcome"] != "failed" || data["by"] != rt.by {
					t.Errorf("audit = %v, want outcome failed by %s", data, rt.by)
				}
				wantReason(t, rt.call(srv, id), http.StatusConflict, rt.busyReason)
				if len(rn.calls) != 1 {
					t.Fatalf("substrate calls = %d after an immediate retry, want 1", len(rn.calls))
				}

				st.now = st.now.Add(2 * time.Minute) // the claim has aged: the retry takes it over
				if w := rt.call(srv, id); w.Code != rt.retryCode {
					t.Fatalf("retry = %d, want %d: %s", w.Code, rt.retryCode, w.Body.String())
				}
				if _, kept := st.drives[d.ID]; kept || len(rn.calls) != 2 {
					t.Errorf("after the retry kept = %v, substrate calls = %d; want the row gone after two calls", kept, len(rn.calls))
				}
				if data := driveAuditData(t, rec, "drive.member.delete"); data["outcome"] != tc.want {
					t.Errorf("retry audit = %v, want outcome %s", data, tc.want)
				}
			})
		}
	}
}

// A typed refusal is the only definitive no-touch answer through the admin route: in use and untouched are a
// 409 with the claim given up (issue count 2), and a refusal that did not say untouched is the held failure.
func TestMemberDriveAdminDeleteRefusalsAreTypedOnly(t *testing.T) {
	adminCtx := operatorCtx("sub-admin", "admin@corp.example", "admin")
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
		held   bool
	}{
		{"in use", runner.ErrDriveInUse, http.StatusConflict, reasonUserDriveReclaimConflict, false},
		{"untouched", runner.Untouched(runner.ErrDriveNotReclaimable), http.StatusConflict, reasonUserDriveReclaimConflict, false},
		{"terminating", fmt.Errorf("Terminating: %w", runner.ErrDriveNotReclaimable), http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemberDriveStore()
			d := seedMemberDrive(st, aliceSub, "Notes")
			rn := &memberDriveRunner{reclaimRunner: reclaimRunner{err: tc.err}}
			srv, rec := memberDriveServer(st, rn, "docker")
			w := memberCall(adminCtx, srv.handleDeleteMemberDriveAdmin, http.MethodDelete, "/x", "", map[string]string{"id": d.ID.String()})
			body := wantReason(t, w, tc.status, tc.reason)
			if strings.Contains(body.Error, "Nothing was removed") {
				t.Errorf("message = %q promises the files survived", body.Error)
			}
			got := st.drives[d.ID]
			if got.Deleting() != tc.held || tc.held && got.DeleteIssues != 1 || !tc.held && got.DeleteIssues != 2 {
				t.Errorf("deleting = %v, issues = %d; want held = %v", got.Deleting(), got.DeleteIssues, tc.held)
			}
			want := "refused"
			if tc.held {
				want = "failed"
			}
			if data := driveAuditData(t, rec, "drive.member.delete"); data["outcome"] != want || data["by"] != "admin" {
				t.Errorf("audit = %v, want outcome %s by admin", data, want)
			}
		})
	}
}

// The same boundary on a disk drive: a Mac's successful reply with no outcome or one this server does not know
// leaves the row held at the issued count with a failed audit (the image may survive), and an error after a partial
// remove does the same; the retry's already_absent answer then removes the row.
func TestMacDiskDeleteHoldsWhatItCannotConfirm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first macReclaimReply
	}{
		{"empty reply", macReclaimReply{}},
		{"unrecognised reply", macReclaimReply{res: runnerwire.ReclaimDriveResult{Outcome: "purged"}}},
		{"partial remove", macReclaimReply{err: errors.New("remove lock: permission denied")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, tr, d := macDiskDeleteRig(t)
			tr.replies = []macReclaimReply{tc.first, {res: runnerwire.ReclaimDriveResult{Outcome: runner.DriveReclaimAlreadyAbsent}}}
			rec := srv.cfg.Audit.(*recRecorder)
			tomb, started, err := st.BeginMemberDriveDelete(context.Background(), aliceSub, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			res := srv.deleteMemberDrive(context.Background(), st, tomb, started, memberDriveByOwner)
			if res.reason != reasonMemberDriveDeleteFailed || res.outcome != driveReclaimOutcomeFailed || res.err != nil {
				t.Fatalf("deletion = %+v, want the ambiguous failure", res)
			}
			got, kept := st.drives[d.ID]
			if !kept || !got.Deleting() || got.DeleteIssues != 1 || tr.asked != 1 {
				t.Fatalf("kept = %v, deleting = %v, issues = %d, Mac asked %d; want held at one issue after one ask", kept, got.Deleting(), got.DeleteIssues, tr.asked)
			}
			if data := driveAuditData(t, rec, "drive.member.delete"); data["outcome"] != "failed" {
				t.Errorf("audit = %v, want outcome failed", data)
			}
			st.now = st.now.Add(2 * time.Minute)
			tomb, started, err = st.BeginMemberDriveDelete(context.Background(), aliceSub, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if res := srv.deleteMemberDrive(context.Background(), st, tomb, started, memberDriveByOwner); res.reason != "" {
				t.Fatalf("retry = %+v, want success", res)
			}
			if _, kept := st.drives[d.ID]; kept || tr.asked != 2 {
				t.Errorf("after the retry kept = %v, Mac asked %d; want the row gone after two asks", kept, tr.asked)
			}
		})
	}
}
