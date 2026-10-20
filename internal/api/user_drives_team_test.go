// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// teamDrivesSite is a site config with storage.user_drive.team_drives on or off.
func teamDrivesSite(on bool) types.SiteConfig {
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{
		Storage: &types.StorageProviders{UserDrive: &types.UserDriveProvider{TeamDrives: on}}}}
}

func teamDrive() (types.UserDrive, types.UserDriveGrant) {
	d := types.UserDrive{ID: uuid.New(), Name: "Team Data", Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, Writable: true, Reclaim: types.DriveReclaimRetain, Team: true}
	g := types.UserDriveGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: "platform", DriveID: d.ID, Enabled: true}
	return d, g
}

// TestTeamDriveResolvesOneReadOnlyFolder: with the switch on, every member of
// the group resolves ONE folder, carrying no person's fingerprint, mounted under
// /home/agent/shared, and read-only until the admin ticks members_can_write; a
// run asking to write it is refused.
func TestTeamDriveResolvesOneReadOnlyFolder(t *testing.T) {
	d, g := teamDrive()
	on := driveSizeCeiling{TeamDrives: true}
	alice, err := newResolvedDrive(&d, &g, types.CapabilitySubjectGroup, []string{"sub-alice"}, on)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := newResolvedDrive(&d, &g, types.CapabilitySubjectGroup, []string{"sub-bob"}, on)
	if err != nil {
		t.Fatal(err)
	}
	if !alice.Team || alice.ObjectName != bob.ObjectName || alice.HomeName != types.DriveTeamHome(d) || alice.SubjectHash != "" {
		t.Fatalf("alice %+v / bob %+v: want one team folder %s and no subject hash", alice, bob, types.DriveTeamHome(d))
	}
	for _, legacy := range []bool{false, true} {
		if got := runner.DriveTargetFor(*alice, legacy); got != runner.SharedRoot+"/team-data" {
			t.Errorf("legacy=%v target = %q, want %s/team-data", legacy, got, runner.SharedRoot)
		}
	}
	srv := New(Config{BaseCtx: testBaseCtx(t), RunnerTarget: "docker", LocalMode: true})
	mount, ok := srv.driveMountFor(context.Background(), httptest.NewRecorder(),
		runDrive{resolved: *alice, target: runner.DriveTargetFor(*alice, false)})
	if !ok || !mount.Team || mount.SubjectHash != "" || mount.Target != runner.SharedRoot+"/team-data" {
		t.Fatalf("mount = %+v (ok %v), want a team mount with no subject hash under %s", mount, ok, runner.SharedRoot)
	}
}

// TestTeamDriveAllocationIsReadOnlyByDefault: a writable team drive allocated
// without members_can_write resolves read-only; ticking it makes it writable.
func TestTeamDriveAllocationIsReadOnlyByDefault(t *testing.T) {
	d, g := teamDrive()
	on := driveSizeCeiling{TeamDrives: true}
	ro, err := newResolvedDrive(&d, &g, types.CapabilitySubjectGroup, []string{"sub-alice"}, on)
	if err != nil || ro.Writable {
		t.Fatalf("default allocation: %+v, %v; want read-only", ro, err)
	}
	g.MembersCanWrite = true
	rw, err := newResolvedDrive(&d, &g, types.CapabilitySubjectGroup, []string{"sub-alice"}, on)
	if err != nil || !rw.Writable {
		t.Fatalf("members_can_write: %+v, %v; want writable", rw, err)
	}
}

// TestTeamDriveMemberWriteIsRefused: without members_can_write a member's run
// mounts the folder read-only, and a run asking to write it is refused.
func TestTeamDriveMemberWriteIsRefused(t *testing.T) {
	d, g := teamDrive()
	resolved, err := newResolvedDrive(&d, &g, types.CapabilitySubjectGroup, []string{"sub-alice"}, driveSizeCeiling{TeamDrives: true})
	if err != nil {
		t.Fatal(err)
	}
	no := false
	if driveReadOnlyRefusal(&no, *resolved) == nil {
		t.Fatal("a run asking to write a read-only team drive was not refused")
	}
	srv := New(Config{BaseCtx: testBaseCtx(t), RunnerTarget: "docker", LocalMode: true})
	mount, ok := srv.driveMountFor(context.Background(), httptest.NewRecorder(),
		runDrive{resolved: *resolved, target: runner.DriveTargetFor(*resolved, false)})
	if !ok || !mount.ReadOnly {
		t.Fatalf("mount = %+v (ok %v), want read-only", mount, ok)
	}
}

// TestTeamDriveSwitchOffResolvesPerPerson: with storage.user_drive.team_drives
// off a team row is today's drive: one folder per person, the drive's own
// posture, and the person's own path.
func TestTeamDriveSwitchOffResolvesPerPerson(t *testing.T) {
	d, g := teamDrive()
	off := driveSizeCeiling{}
	alice, err := newResolvedDrive(&d, &g, types.CapabilitySubjectGroup, []string{"sub-alice"}, off)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := newResolvedDrive(&d, &g, types.CapabilitySubjectGroup, []string{"sub-bob"}, off)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := types.DriveHomeName(d, "sub-alice", "")
	if alice.Team || alice.HomeName != want || alice.HomeName == bob.HomeName || alice.SubjectHash == "" || !alice.Writable {
		t.Fatalf("alice %+v / bob %+v: want per-person homes as today", alice, bob)
	}
	if got := runner.DriveTargetFor(*alice, true); got != runner.DriveTarget {
		t.Errorf("legacy target = %q, want %s", got, runner.DriveTarget)
	}
}

// TestTeamDriveOtherTierMountsNothing: a team drive allocated to anything but a
// group (a row written around the write boundary) mounts nothing.
func TestTeamDriveOtherTierMountsNothing(t *testing.T) {
	d, g := teamDrive()
	for _, tier := range []types.CapabilitySubjectType{types.CapabilitySubjectUser, types.CapabilitySubjectAll, types.CapabilitySubjectUserType} {
		g.SubjectType = tier
		if _, err := newResolvedDrive(&d, &g, tier, []string{"sub-alice"}, driveSizeCeiling{TeamDrives: true}); !errors.Is(err, errDriveUnmountable) {
			t.Errorf("tier %s: err = %v, want errDriveUnmountable", tier, err)
		}
	}
}

// TestTeamDriveMountAuditNamesThePerson: a team drive's run.drive.mount row names
// the person whose run mounted it; a per-person drive's row is unchanged.
func TestTeamDriveMountAuditNamesThePerson(t *testing.T) {
	audit := &recRecorder{}
	srv := New(Config{BaseCtx: testBaseCtx(t), Audit: audit, LocalMode: true})
	srv.auditDriveMount(context.Background(), uuid.New(), "sub-alice", []types.DriveMount{
		{DriveID: uuid.New(), Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-drive-x-t-1", Target: runner.SharedRoot + "/team", ReadOnly: true, Team: true},
		{DriveID: uuid.New(), Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-drive-y-d-1", Target: runner.DriveTarget},
	})
	if len(audit.events) != 2 {
		t.Fatalf("rows = %d, want 2", len(audit.events))
	}
	var team, own map[string]any
	_ = json.Unmarshal(audit.events[0].Data, &team)
	_ = json.Unmarshal(audit.events[1].Data, &own)
	if team["mounted_by"] != "sub-alice" || team["team"] != true || team["mode"] != "ro" {
		t.Errorf("team row = %v, want mounted_by sub-alice, team, ro", team)
	}
	if _, ok := own["mounted_by"]; ok || len(own) != 7 {
		t.Errorf("per-person row = %v, want today's seven fields", own)
	}
}

// TestTeamDriveReclaimSkipsIt: a person's reclaim (the leaver's cleanup) is
// refused on a team drive while team drives are on, and reaches no storage; off,
// the drive reclaims per person as today.
func TestTeamDriveReclaimSkipsIt(t *testing.T) {
	d, _ := teamDrive()
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, st, audit := reclaimServer(t, d, nil, rn)
	st.site = teamDrivesSite(true)
	w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusUnprocessableEntity || len(rn.calls) != 0 || len(audit.events) != 0 {
		t.Fatalf("reclaim of a team drive = %d (%s), calls %d, rows %d; want 422 and nothing touched",
			w.Code, w.Body.String(), len(rn.calls), len(audit.events))
	}
	st.site = teamDrivesSite(false)
	if w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`); w.Code != http.StatusOK || len(rn.calls) != 1 {
		t.Fatalf("switch off: reclaim = %d (%s), calls %d; want today's per-person reclaim", w.Code, w.Body.String(), len(rn.calls))
	}
}

// TestTeamDriveAdminWrites: marking a drive a team drive needs the switch; a
// team drive is allocated to groups only; team cannot flip on an allocated
// drive; and GET /drives says a Docker-volume team drive is on one host.
func TestTeamDriveAdminWrites(t *testing.T) {
	st := newDriveCRUDStore()
	srv, rec := driveAdminServer(st, nil)
	body := `{"name":"Team Data","backend":"docker_volume","team":true}`
	if w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body, nil); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("team drive with the switch off = %d (%s), want 422", w.Code, w.Body.String())
	}
	st.site = teamDrivesSite(true)
	w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("team drive with the switch on = %d (%s), want 201", w.Code, w.Body.String())
	}
	var saved types.UserDrive
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || !saved.Team {
		t.Fatalf("saved = %+v, %v; want team", saved, err)
	}
	if data := driveAuditData(t, rec, "drive.write"); data["team"] != true {
		t.Errorf("drive.write = %v, want team", data)
	}
	grant := func(subjectType, extra string) *httptest.ResponseRecorder {
		return driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
			`{"subject_type":"`+subjectType+`","subject":"platform","drive_id":"`+saved.ID.String()+`","priority":0`+extra+`}`, nil)
	}
	if w := grant("user", ""); w.Code != http.StatusBadRequest {
		t.Errorf("user allocation of a team drive = %d (%s), want 400", w.Code, w.Body.String())
	}
	if w := grant("group", `,"members_can_write":true`); w.Code != http.StatusBadRequest {
		t.Errorf("members_can_write on a read-only team drive = %d (%s), want 400", w.Code, w.Body.String())
	}

	st.grants[uuid.New()] = types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "platform", DriveID: saved.ID, Enabled: true}
	w = driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+saved.ID.String(),
		`{"name":"Team Data","backend":"docker_volume"}`, map[string]string{"id": saved.ID.String()})
	if w.Code != http.StatusConflict {
		t.Errorf("unsetting team on an allocated drive = %d (%s), want 409", w.Code, w.Body.String())
	}
	w = driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives", "", nil)
	var got userDrivesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Drives) != 1 || !got.Drives[0].SingleHost {
		t.Fatalf("GET /drives = %s, want the team drive marked single_host", w.Body.String())
	}
}
