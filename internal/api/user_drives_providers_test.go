// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// providerSiteConfig is a SiteConfig carrying only the storage.user_drive block
// — the org switch and the deployment's drive ceiling, which is all any test
// here cares about.
func providerSiteConfig(disabled bool, maxSizeMiB int) types.SiteConfig {
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{
		Storage: &types.StorageProviders{
			UserDrive: &types.UserDriveProvider{Disabled: disabled, MaxSizeMiB: maxSizeMiB},
		},
	}}
}

// TestDriveWritesMeetTheOrgSwitchAndTheCeiling walks the two admin write doors
// against storage.user_drive.
//
// Both refusals are 422 and neither is a 403, which is the whole classification:
// nobody was denied by a profile. The org switch says this install offers no
// drives at all, and the ceiling says the deployment will not hold a drive that
// big — in decodeUserDriveRequest's own words, "a 400 says you wrote this wrong,
// a 422 says there is nothing here to write it into".
func TestDriveWritesMeetTheOrgSwitchAndTheCeiling(t *testing.T) {
	driveID := uuid.New()
	grantBody := func(sizeOverride int) string {
		return fmt.Sprintf(`{"subject_type":"user","subject":"sub-bob","drive_id":%q,"size_mib_override":%d}`,
			driveID, sizeOverride)
	}

	for _, tc := range []struct {
		name string
		site types.SiteConfig
		// call runs one write door against srv and returns its answer.
		call func(t *testing.T, srv *Server) *httptest.ResponseRecorder
		code int
		want string
	}{
		{
			name: "a drive write while drives are disabled",
			site: providerSiteConfig(true, 0),
			call: func(t *testing.T, srv *Server) *httptest.ResponseRecorder {
				return driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", driveCreateBody, nil)
			},
			code: http.StatusUnprocessableEntity,
			want: driveDisabledMsg,
		},
		{
			name: "an ALLOCATION while drives are disabled — the switch is not about the number",
			site: providerSiteConfig(true, 0),
			call: func(t *testing.T, srv *Server) *httptest.ResponseRecorder {
				return driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants", grantBody(0), nil)
			},
			code: http.StatusUnprocessableEntity,
			want: driveDisabledMsg,
		},
		{
			name: "a drive above the deployment ceiling",
			site: providerSiteConfig(false, 1024),
			call: func(t *testing.T, srv *Server) *httptest.ResponseRecorder {
				return driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", driveCreateBody, nil)
			},
			code: http.StatusUnprocessableEntity,
			want: fmt.Sprintf(driveCeilingMsg, 10240, 1024),
		},
		{
			// The PROVIDER ceiling only, and that is the decision: a group or
			// `all` allocation has no single principal, so the per-principal
			// governance ceiling cannot be resolved at this door at all — it is
			// clamped at resolve instead.
			name: "an override above the deployment ceiling",
			site: providerSiteConfig(false, 1024),
			call: func(t *testing.T, srv *Server) *httptest.ResponseRecorder {
				return driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants", grantBody(4096), nil)
			},
			code: http.StatusUnprocessableEntity,
			want: fmt.Sprintf(driveCeilingMsg, 4096, 1024),
		},
		{
			// The control: a ceiling the write is INSIDE refuses nothing, and an
			// override of 0 is "unset", never "smaller than every ceiling".
			name: "an allocation under the ceiling is written",
			site: providerSiteConfig(false, 1024),
			call: func(t *testing.T, srv *Server) *httptest.ResponseRecorder {
				return driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants", grantBody(0), nil)
			},
			code: http.StatusCreated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDriveCRUDStore()
			st.site = tc.site
			st.drives[driveID] = types.UserDrive{
				ID: driveID, Name: "Corp NAS", Backend: types.DriveBackendDockerVolume,
				HomeTemplate: types.HomeTemplateHash, SizeMiB: 512, Reclaim: types.DriveReclaimRetain,
			}
			srv, _ := driveAdminServer(st, nil)

			w := tc.call(t, srv)
			if w.Code != tc.code {
				t.Fatalf("code = %d, want %d — the deployment refuses this, no profile denied anybody: %s",
					w.Code, tc.code, w.Body.String())
			}
			if tc.want != "" && !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body = %s\nwant it to carry %q", w.Body.String(), tc.want)
			}
		})
	}
}

// TestGetUserDrivesReportsTheOrgSwitch pins the field the console's banner is
// drawn from. It is not derivable from the rows — everything on the screen is
// KEPT when drives are switched off — so the server has to say it.
func TestGetUserDrivesReportsTheOrgSwitch(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		st := newDriveCRUDStore()
		st.site = providerSiteConfig(disabled, 0)
		srv, _ := driveAdminServer(st, nil)

		w := driveCall(t, srv.handleGetUserDrives, http.MethodGet, "/api/v1/drives", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d: %s", w.Code, w.Body.String())
		}
		var got userDrivesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Disabled != disabled {
			t.Errorf("disabled = %v, want %v — the switch is org policy, not a property of the rows", got.Disabled, disabled)
		}
	}
}

// TestSeedRequestDriveRefusesWhenDrivesAreDisabled is the ORDER, and the order is
// the finding: the org switch is asked BEFORE the per-profile door, so a
// deployment with drives switched off answers 422 in the REFUSED_BACKEND family
// and writes NO authz.denied row — nobody was denied, there is nothing here to
// mount. The ceiling handed in is one whose door is ALSO shut, so an
// implementation that asked the door first would answer 403 and log a denial.
func TestSeedRequestDriveRefusesWhenDrivesAreDisabled(t *testing.T) {
	st := &driveStore{site: providerSiteConfig(true, 0)}
	st.drive = driveFixture(nil)
	st.grant = grantFixture(st.drive.ID, nil)
	st.tier = types.CapabilitySubjectUser
	srv, rec := driveRunServer(st, "docker")

	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), deniedCeiling(), driveMemberCtx([]string{"eng"}, false))
	if ok || mount != nil {
		t.Fatalf("mount = %+v, ok = %v; want the run refused its drive", mount, ok)
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("code = %d, want 422 — a 403 would say a profile denied this member, and none did: %s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), driveDisabledMsg) {
		t.Errorf("body = %s, want it to carry %q", w.Body.String(), driveDisabledMsg)
	}
	if len(rec.events) != 0 {
		t.Errorf("audit = %v, want nothing — the org switch is not an authorization event",
			driveAuditActions(rec))
	}
}

// TestDriveSizeIsClampedTheSameAtEveryDoor is the parity the resolver file
// insists on: launch, GET /me and POST /drives/preview read ONE fold, so a
// member cannot be shown 10 GiB on the card, previewed at 10 GiB by their admin,
// and given 2 GiB by the run.
//
// Both ceilings are in PLAY and the smaller one wins: the deployment allows
// 4 GiB, this principal's profile allows 2 GiB, and the allocation is 10 GiB.
func TestDriveSizeIsClampedTheSameAtEveryDoor(t *testing.T) {
	const deploymentMiB, profileMiB = 4096, 2048
	profile := &types.GovernanceProfile{
		Name: "contractors", Limits: types.GovernanceLimits{MaxDriveSizeMiB: profileMiB},
	}
	newStore := func() *driveStore {
		d := driveFixture(nil) // 10240 MiB allocated
		return &driveStore{
			drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser,
			profile: profile, site: providerSiteConfig(false, deploymentMiB),
		}
	}

	// 1. LAUNCH — the mount the runner executes.
	srv, _ := driveRunServer(newStore(), "docker")
	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil),
		governanceCeiling{Profile: profile, Limits: profile.Limits}, driveMemberCtx([]string{"eng"}, false))
	if !ok || mount == nil {
		t.Fatalf("launch: mount = %+v, ok = %v: %s", mount, ok, w.Body.String())
	}
	if mount.SizeMiB != profileMiB {
		t.Errorf("launch size = %d, want %d — the profile is the smaller of the two ceilings", mount.SizeMiB, profileMiB)
	}

	// 2. GET /me — the number on the card.
	meSrv, _ := driveRunServer(newStore(), "docker")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(driveMemberCtx([]string{"eng"}, false))
	me, unavailable := meSrv.resolveMeUserDrive(r)
	if me == nil {
		t.Fatalf("/me: user_drive = nil (%q), want the allocation", unavailable)
	}
	if me.SizeMiB != mount.SizeMiB {
		t.Errorf("/me size = %d, launch size = %d — the card must not offer a size the run will not give",
			me.SizeMiB, mount.SizeMiB)
	}

	// 3. POST /drives/preview — what the admin is shown for the same principal.
	pvSrv, _ := driveRunServer(newStore(), "docker")
	pw := driveCall(t, pvSrv.handlePreviewUserDrive, http.MethodPost, "/api/v1/drives/preview",
		`{"user_subjects":["sub-drive-bob"],"groups":["eng"]}`, nil)
	if pw.Code != http.StatusOK {
		t.Fatalf("preview: code = %d: %s", pw.Code, pw.Body.String())
	}
	var preview userDrivePreviewResponse
	if err := json.Unmarshal(pw.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.SizeMiB != mount.SizeMiB {
		t.Errorf("preview size = %d, launch size = %d — the preview resolves the PREVIEWED principal's ceiling "+
			"(drivePreviewDoorIsOpen's own), so it cannot print a size no launch agrees with",
			preview.SizeMiB, mount.SizeMiB)
	}
}

// TestDrivesDisabledAnswersTheSameAtEveryDoor is TestDriveSizeIsClampedTheSame-
// AtEveryDoor's fixture with the ORG SWITCH thrown, and it is the parity that was
// missing: `disabled` was read at the launch door and at the two admin write
// boundaries, and asked at NEITHER of the surfaces a member's console draws from.
//
// /me shipped `{"name":"Corp NAS","size_mib":…,"writable":…}` with an empty
// user_drive_unavailable, and the preview answered 200 with the same, on a
// deployment whose create path answers 422 "drives are disabled for this
// deployment" — so the New Run card drew the mount checkbox and its writable
// sentence for a mount no run here can have.
//
// THE /me TOKEN IS THE EXISTING `unavailable`, not a fifth member of the closed
// set: the console already renders it (NR_UNAVAILABLE), and a token no client
// branches on yet would leave the card rendering nothing at all. A drives-are-off
// member sentence of its own is a canon item, not a wire one.
func TestDrivesDisabledAnswersTheSameAtEveryDoor(t *testing.T) {
	newStore := func() *driveStore {
		d := driveFixture(nil)
		return &driveStore{
			drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser,
			site: providerSiteConfig(true, 0),
		}
	}
	wantSentence := fmt.Sprintf(driveRefusedBackendMsg, driveDisabledMsg)

	// 1. LAUNCH — unchanged: the 422 the switch has always answered.
	srv, _ := driveRunServer(newStore(), "docker")
	mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil),
		governanceCeiling{}, driveMemberCtx([]string{"eng"}, false))
	if ok || mount != nil {
		t.Fatalf("launch: mount = %+v, ok = %v; want the run refused its drive", mount, ok)
	}
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), driveDisabledMsg) {
		t.Errorf("launch: %d %s, want 422 carrying %q", w.Code, w.Body.String(), driveDisabledMsg)
	}

	// 2. GET /me — NO allocation, and the reason says so on the wire.
	meSrv, _ := driveRunServer(newStore(), "docker")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(driveMemberCtx([]string{"eng"}, false))
	me, unavailable := meSrv.resolveMeUserDrive(r)
	if me != nil {
		t.Errorf("/me: user_drive = %+v, want nil — the card must not offer a mount the create path refuses 422", me)
	}
	if unavailable != driveUnavailableUnknown {
		t.Errorf("/me: user_drive_unavailable = %q, want %q — an empty reason is the affirmative \"nothing is wrong\" this deployment cannot make",
			unavailable, driveUnavailableUnknown)
	}

	// 3. POST /drives/preview — the launch door's OWN bytes, not a paraphrase and
	// not a 200.
	pvSrv, _ := driveRunServer(newStore(), "docker")
	pw := driveCall(t, pvSrv.handlePreviewUserDrive, http.MethodPost, "/api/v1/drives/preview",
		`{"user_subjects":["sub-drive-bob"],"groups":["eng"]}`, nil)
	if pw.Code != http.StatusUnprocessableEntity {
		t.Errorf("preview: code = %d, want 422 in the REFUSED_BACKEND family: %s", pw.Code, pw.Body.String())
	}
	if !strings.Contains(pw.Body.String(), wantSentence) {
		t.Errorf("preview: body = %s\nwant the launch door's own sentence %q", pw.Body.String(), wantSentence)
	}
}

// TestDrivesDisabledBeatsTheProfileDoorAtEverySurface is the ORDER, at the two
// surfaces that resolve a door: 422 before 403, because the org switch and
// DenyUserDrive answer different questions and only the second is about a member.
// With drives off nobody was denied — there is nothing here to mount — so a 403
// would name a member (or, at the preview, tell an admin about one) that no
// profile refused, and write an authz.denied row for it.
func TestDrivesDisabledBeatsTheProfileDoorAtEverySurface(t *testing.T) {
	newStore := func() *driveStore {
		d := driveFixture(nil)
		return &driveStore{
			drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser,
			profile: &types.GovernanceProfile{
				Name: "contractors", Limits: types.GovernanceLimits{DenyUserDrive: true},
			},
			site: providerSiteConfig(true, 0),
		}
	}

	srv, rec := driveRunServer(newStore(), "docker")
	_, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), deniedCeiling(),
		driveMemberCtx([]string{"eng"}, false))
	if ok || w.Code != http.StatusUnprocessableEntity {
		t.Errorf("launch: ok = %v code = %d, want 422 — a 403 would log a denial nobody made: %s",
			ok, w.Code, w.Body.String())
	}
	if len(rec.events) != 0 {
		t.Errorf("launch audit = %v, want nothing — the org switch is not an authorization event",
			driveAuditActions(rec))
	}

	pvSrv, pvRec := driveRunServer(newStore(), "docker")
	pw := driveCall(t, pvSrv.handlePreviewUserDrive, http.MethodPost, "/api/v1/drives/preview",
		`{"user_subjects":["sub-drive-bob"],"groups":["eng"]}`, nil)
	if pw.Code != http.StatusUnprocessableEntity {
		t.Errorf("preview: code = %d, want 422 — the switch is asked before the previewed principal's door: %s",
			pw.Code, pw.Body.String())
	}
	if len(pvRec.events) != 0 {
		t.Errorf("preview audit = %v, want nothing", driveAuditActions(pvRec))
	}
}

// TestDriveRehomeRaceIsRefusedAtTheWrite closes the window a read-then-write gate
// leaves: the gate reads the allocations, finds none, and the write lands AFTER
// somebody is allocated the drive — re-homing them silently.
//
// grantsAppear makes that happen deterministically, inside the write itself. The
// precondition rides the statement, so the write is refused rather than applied,
// and the drive keeps its old identity fields.
func TestDriveRehomeRaceIsRefusedAtTheWrite(t *testing.T) {
	st := newDriveCRUDStore()
	id := uuid.New()
	st.drives[id] = types.UserDrive{
		ID: id, Name: "Corp NAS", Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, SizeMiB: 10240, Reclaim: types.DriveReclaimRetain,
	}
	// The allocation lands in the window: after driveRehomeGuard's read, before
	// the statement.
	st.grantsAppear = func() {
		gid := uuid.New()
		st.grants[gid] = types.UserDriveGrant{
			ID: gid, SubjectType: types.CapabilitySubjectUser, Subject: "sub-bob", DriveID: id, Enabled: true,
		}
	}
	srv, rec := driveAdminServer(st, nil)

	// A RENAME, which is identity-affecting: both minted object names carry the
	// drive slug.
	w := driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+id.String(),
		`{"name":"Corp NAS 2","backend":"docker_volume","size_mib":10240}`, map[string]string{"id": id.String()})
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 — somebody was allocated this drive while the write was in flight: %s",
			w.Code, w.Body.String())
	}
	if got := st.drives[id].Name; got != "Corp NAS" {
		t.Errorf("stored name = %q, want the write to have applied NOTHING — a re-home nobody confirmed", got)
	}
	if len(rec.events) != 0 {
		t.Errorf("audit = %v, want no drive.write row for a write that did not happen", driveAuditActions(rec))
	}
}
