// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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
		audit   string
	}{
		{"deleted", runner.DriveReclaimDeleted, nil, http.StatusNoContent, "", false, "deleted"},
		{"already absent", runner.DriveReclaimAlreadyAbsent, nil, http.StatusNoContent, "", false, "already_absent"},
		{"in use", "", runner.ErrDriveInUse, http.StatusConflict, reasonMemberDriveInUse, true, "refused"},
		{"not this drive's", "", runner.ErrDriveNotReclaimable, http.StatusConflict, reasonMemberDriveInUse, true, "refused"},
		{"substrate failed", "", errors.New("daemon down"), http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, true, "failed"},
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
				wantReason(t, w, tc.status, tc.reason)
			}
			home, _ := types.DriveHomeName(d, aliceSub, "")
			if len(rn.calls) != 1 || rn.calls[0].ObjectName != types.DriveObjectName(d, home) ||
				rn.calls[0].SubjectHash != types.DriveSubjectHash(aliceSub) || rn.calls[0].DriveID != d.ID {
				t.Fatalf("substrate calls = %+v, want one for the owner's object", rn.calls)
			}
			got, kept := st.drives[d.ID]
			if kept != tc.kept || kept && got.DeleteStartedAt != nil {
				t.Errorf("row kept = %v (tombstone %v), want kept = %v with the tombstone cleared", kept, got.DeleteStartedAt, tc.kept)
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

// TestMemberDriveDeleteWaitsOutAStartingRun: a run of the owner's still STARTING may have re-read the
// drive before the tombstone and make its object after a reclaim, so the delete answers in use, keeps
// the drive with the tombstone cleared and never reaches the substrate. Another person's STARTING run
// does not hold it.
func TestMemberDriveDeleteWaitsOutAStartingRun(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	st.starting = map[string]bool{aliceSub: true}
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, rec := memberDriveServer(st, rn, "docker")
	wantReason(t, deleteMyDrive(srv, aliceSub, d.ID.String()), http.StatusConflict, reasonMemberDriveInUse)
	if got, kept := st.drives[d.ID]; !kept || got.DeleteStartedAt != nil || len(rn.calls) != 0 {
		t.Fatalf("kept = %v (tombstone %v), substrate calls = %d; want kept, tombstone cleared, no call", kept, got.DeleteStartedAt, len(rn.calls))
	}
	if data := driveAuditData(t, rec, "drive.member.delete"); data["outcome"] != "refused" {
		t.Errorf("audit = %v, want outcome refused", data)
	}
	st.starting = map[string]bool{bobSub: true}
	if w := deleteMyDrive(srv, aliceSub, d.ID.String()); w.Code != http.StatusNoContent || len(rn.calls) != 1 {
		t.Fatalf("delete beside another person's starting run = %d (calls %d), want 204 after one reclaim: %s", w.Code, len(rn.calls), w.Body.String())
	}
}

// TestAdminListAndDeleteMemberDrives: the super admin lists every person's
// drive with its owner and object, and deletes one with by=admin.
func TestAdminListAndDeleteMemberDrives(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	seedMemberDrive(st, bobSub, "Logs")
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, rec := memberDriveServer(st, rn, "docker")
	adminCtx := operatorCtx("sub-admin", "admin@corp.example", "admin")
	w := memberCall(adminCtx, srv.handleListMemberDrives, http.MethodGet, "/api/v1/drives/member", "", nil)
	var list client.MemberDriveList
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Total != 2 || len(list.Drives) != 2 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
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
