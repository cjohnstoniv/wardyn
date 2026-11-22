// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A drive held since the upgrade is listed (it counts toward the limit and can be deleted by its owner) but a run
// cannot attach it: the create refuses with drive_not_found and the sentence for its backend.
func TestLegacyHeldDriveIsListedButNotAttachable(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	d.LegacyUnsettled = true
	st.drives[d.ID] = d
	// The delete at the end needs a substrate that confirms it (a reply with no outcome keeps the row held).
	srv, _ := memberDriveServer(st, &memberDriveRunner{reclaimRunner: reclaimRunner{Runner: &fakeRunner{}, outcome: runner.DriveReclaimDeleted}}, "docker")
	alice := personCtx(aliceSub)

	got := meDrivesOf(t, srv, alice)
	if len(got.Drives) != 1 || got.Create == nil || got.Create.Count != 1 {
		t.Fatalf("catalogue = %+v (create %+v), want the held drive listed and counted", got.Drives, got.Create)
	}
	_, f := authorizeDrives(srv, alice, drivesRequest(d.ID.String()))
	const want = "Drive Notes is waiting for an administrator to finish the upgrade and cannot be used yet."
	if f == nil || f.body.Reason != string(authz.ReasonDriveNotFound) || f.body.Error != want {
		t.Errorf("attaching a held drive = %+v, want drive_not_found with %q", f, want)
	}
	if w := deleteMyDrive(srv, aliceSub, d.ID.String()); w.Code != http.StatusNoContent {
		t.Errorf("deleting a held drive = %d %s, want 204", w.Code, w.Body)
	}
	if _, kept := st.drives[d.ID]; kept {
		t.Error("the held drive is still there after its owner deleted it")
	}
}

// A Mac's held disk is named in its own words.
func TestLegacyHeldDriveSentences(t *testing.T) {
	mac := types.UserDrive{Name: "Disk", Backend: types.DriveBackendMacDisk}
	docker := types.UserDrive{Name: "Vol", Backend: types.DriveBackendDockerVolume}
	if got, want := legacyDriveHeldMsg(mac), "Drive Disk is waiting for its Mac to be upgraded and restarted."; got != want {
		t.Errorf("Mac sentence = %q, want %q", got, want)
	}
	if got, want := legacyDriveHeldMsg(docker), "Drive Vol is waiting for an administrator to finish the upgrade and cannot be used yet."; got != want {
		t.Errorf("Docker sentence = %q, want %q", got, want)
	}
}

// consumedIssueStore answers every issue as a claim that was consumed or taken over.
type consumedIssueStore struct{ *memberDriveStore }

func (consumedIssueStore) IssueMemberDriveDelete(context.Context, uuid.UUID, time.Time, int) (int, error) {
	return 0, store.ErrNotFound
}

// An issue that finds its claim gone answers 409 member_drive_busy and makes no call to the substrate; the row is
// left to whoever holds the claim now.
func TestMemberDriveDeleteWhoseClaimWasTakenMakesNoCall(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: "deleted"}}
	srv, _ := memberDriveServer(st, rn, "docker")
	srv.cfg.Store = consumedIssueStore{st}
	wantReason(t, deleteMyDrive(srv, aliceSub, d.ID.String()), http.StatusConflict, reasonMemberDriveBusy)
	if len(rn.calls) != 0 {
		t.Errorf("substrate calls = %d, want none", len(rn.calls))
	}
	if got, kept := st.drives[d.ID]; !kept || got.DeleteStartedAt == nil {
		t.Errorf("kept = %v, claim %v; want the row left marked for its holder", kept, got.DeleteStartedAt)
	}
}
