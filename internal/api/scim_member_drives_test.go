// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
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
	if got := fmt.Sprint(d["granted_drives"]); got != "[alpha zeta]" {
		t.Errorf("granted_drives = %s, want only the allocated names, so the status can tell them from the created ones", got)
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

func scimDeprovisionRow(person uuid.UUID, at time.Time, data string) types.AuditEvent {
	return scimCovAudit("person.deprovision", person.String(), at, data)
}

// TestSCIMStatusDropsADeletedMemberDriveBesideAnAdminDriveOfItsName: an admin
// drive the person had no grant on shares the name of the drive they created.
// Once the created one is deleted, nothing of theirs is left to reclaim.
func TestSCIMStatusDropsADeletedMemberDriveBesideAnAdminDriveOfItsName(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(store.PrincipalIdentity{Principal: "sub-pat", EmailLower: "pat@corp.example", ScimUserName: "Pat"})
	mine := scimMemberDrive("sub-pat", "Notes")
	admin := types.UserDriveListItem{UserDrive: types.UserDrive{ID: uuid.New(), Name: "Notes"}}
	st.drives = []types.UserDriveListItem{admin, mine}
	at := time.Now().UTC().Truncate(time.Second)
	row := scimDeprovisionRow(pat.ID, at, fmt.Sprintf(`{"kind":"purge","drives":["Notes"],"granted_drives":[],"member_drive_ids":[%q]}`, mine.ID))
	e, _ := scimCovPagedEnv(t, st, []types.AuditEvent{row})

	if _, body := e.status(); len(body.Drives) != 1 {
		t.Fatalf("drives = %+v, want Pat's created Notes", body.Drives)
	}
	st.drives = []types.UserDriveListItem{admin} // the operator deletes Pat's drive
	if _, body := e.status(); len(body.Drives) != 0 {
		t.Errorf("drives = %+v, want none: the admin drive of that name was never Pat's", body.Drives)
	}
}

// TestSCIMStatusKeepsALiveMemberDriveAfterTheAdminDriveIsReclaimed: the person
// held a grant on an admin drive and created one of the same name. Reclaiming
// the granted one takes it off the list; the created one, still live, stays.
func TestSCIMStatusKeepsALiveMemberDriveAfterTheAdminDriveIsReclaimed(t *testing.T) {
	st := newSCIMCovStore()
	pat := st.addIdentity(store.PrincipalIdentity{Principal: "sub-pat", EmailLower: "pat@corp.example", ScimUserName: "Pat"})
	mine := scimMemberDrive("sub-pat", "Notes")
	admin := types.UserDriveListItem{UserDrive: types.UserDrive{ID: uuid.New(), Name: "Notes"}}
	st.drives = []types.UserDriveListItem{admin, mine}
	at := time.Now().UTC().Truncate(time.Second)
	row := scimDeprovisionRow(pat.ID, at, fmt.Sprintf(`{"kind":"purge","drives":["Notes"],"granted_drives":["Notes"],"member_drive_ids":[%q]}`, mine.ID))
	reclaim := types.AuditEvent{
		ID: uuid.New(), Time: at.Add(time.Hour), ActorType: types.ActorHuman, Actor: "admin", Action: "drive.reclaim", Outcome: "success",
		Data: json.RawMessage(`{"drive":"Notes","subject":"pat@corp.example","subject_type":"user"}`),
	}
	e, paged := scimCovPagedEnv(t, st, []types.AuditEvent{row})

	if _, body := e.status(); len(body.Drives) != 1 {
		t.Fatalf("drives = %+v, want one Notes entry before the reclaim", body.Drives)
	}
	paged.audit = []types.AuditEvent{row, reclaim}
	if _, body := e.status(); len(body.Drives) != 1 || body.Drives[0].Drive != "Notes" {
		t.Errorf("drives = %+v, want Pat's created Notes still listed once the granted one is reclaimed", body.Drives)
	}
}

// TestSCIMPurgeRecordsAnEmptyGrantedListAsEmptyNotNull: a person who only created
// drives has no allocated ones. The row must say so with [], because the status
// reads a missing or null granted_drives as a row from before the field existed
// and would then take the created drive's name for an allocated one.
func TestSCIMPurgeRecordsAnEmptyGrantedListAsEmptyNotNull(t *testing.T) {
	st := newSCIMCovStore()
	l := scimCovSeedPurgee(st)
	mine := scimMemberDrive("sub-pat", "Notes")
	st.drives, st.grants = []types.UserDriveListItem{mine}, nil
	e := newSCIMCovEnv(t, st)

	if w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
	}
	d := e.auditData(e.purgeRow())
	if granted, ok := d["granted_drives"].([]any); !ok || len(granted) != 0 {
		t.Errorf("granted_drives = %#v, want [] (present and empty, never null)", d["granted_drives"])
	}
	if got := fmt.Sprint(d["drives"]); got != "[Notes]" {
		t.Errorf("drives = %s, want the created drive's name", got)
	}
}
