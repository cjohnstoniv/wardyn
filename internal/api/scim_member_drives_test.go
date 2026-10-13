// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func scimMemberDrive(owner, name string) types.UserDriveListItem {
	return types.UserDriveListItem{UserDrive: types.UserDrive{ID: uuid.New(), Name: name, OwnerSubject: owner}}
}

// TestSCIMPurgeListsMemberDrives: a purge lists the drives the person created
// beside the ones allocated to them, once each by name and by id, tombstoned
// ones included, and no one else's; and it deletes none.
func TestSCIMPurgeListsMemberDrives(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	mine, tomb := scimMemberDrive("sub-pat", "Notes"), scimMemberDrive("sub-pat", "Scratch")
	now := time.Now()
	tomb.DeleteStartedAt = &now
	// Same name as the person's, owned by someone else: neither listed nor read as theirs.
	theirs := scimMemberDrive("sub-bystander", "Notes")
	st.drives = append(st.drives, mine, tomb, theirs)
	e := newSCIMCovEnv(t, st)
	before := len(st.drives)

	if w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
	}
	d := e.auditData(e.purgeRow())
	if got := fmt.Sprint(d["drives"]); got != "[Notes Scratch alpha zeta]" {
		t.Errorf("drives = %s, want the person's allocations and their own drives once each, sorted", got)
	}
	want := []string{mine.ID.String(), tomb.ID.String()}
	slices.Sort(want)
	if got := fmt.Sprint(d["member_drive_ids"]); got != fmt.Sprint(want) {
		t.Errorf("member_drive_ids = %s, want %v: theirs only", got, want)
	}
	if len(st.drives) != before {
		t.Errorf("the purge deleted a drive (%d -> %d): it only lists", before, len(st.drives))
	}
}

// TestSCIMStatusFollowsMemberDrivesById: a member drive a purge listed stays on
// the operator's list until it is deleted, and another person's drive of the
// same name keeps neither it nor an admin drive of that name on the list.
func TestSCIMStatusFollowsMemberDrivesById(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(store.PrincipalIdentity{Principal: "sub-pat", EmailLower: "pat@corp.example", ScimUserName: "Pat"})
	mine := scimMemberDrive("sub-pat", "Notes")
	gone := scimMemberDrive("sub-pat", "Gone")
	theirs := scimMemberDrive("sub-bystander", "Notes")
	// "Gone" is not live: it was deleted after the purge.
	st.drives = []types.UserDriveListItem{mine, theirs}
	at := time.Now().UTC().Truncate(time.Second)
	row := scimCovAudit("person.deprovision", pat.ID.String(), at,
		fmt.Sprintf(`{"kind":"purge","drives":["Gone","Notes"],"member_drive_ids":[%q,%q]}`, gone.ID, mine.ID))
	e, _ := scimCovPagedEnv(t, st, []types.AuditEvent{row})

	w, body := e.status()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	if len(body.Drives) != 1 || body.Drives[0].Person != "Pat" || body.Drives[0].Drive != "Notes" {
		t.Fatalf("drives = %+v, want Pat's Notes alone", body.Drives)
	}

	// Pat's drive is deleted. The bystander's Notes shares its name and stays live.
	st.drives = []types.UserDriveListItem{theirs}
	if _, body := e.status(); len(body.Drives) != 0 {
		t.Errorf("drives = %+v, want none once the person's drive is deleted, whatever else holds the name", body.Drives)
	}
}
