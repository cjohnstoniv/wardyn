// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	carolSub = "sub-member-carol" // in the child of the team drive's unit
	daveSub  = "sub-member-dave"  // in its sibling
	erinSub  = "sub-member-erin"  // in its parent
	frankSub = "sub-member-frank" // in Default
)

// teamFacts is a fake row's team_unit_id and team_members_can_write.
type teamFacts struct {
	unit  *uuid.UUID
	write bool
}

// teamDriveStore is the member-drive double with a unit tree and people's own team drives: unitOf is each person's
// unit (a person not in it is in none), team the team facts of each team row, and anyStarting a run of anyone's in
// STARTING beside the per-owner starting map.
type teamDriveStore struct {
	*memberDriveStore
	units       []types.GovernanceUnit
	unitOf      map[string]uuid.UUID
	team        map[uuid.UUID]*teamFacts
	anyStarting bool
	// failedRuns counts the runs a failed re-read asked to fail (UpdateRunStateIf); none is applied.
	failedRuns int
}

func (s *teamDriveStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	s.failedRuns++
	return false, nil
}

// teamTree is Engineering with Platform and Data under it, Platform SRE under Platform, and Default.
type teamTree struct{ parent, unit, child, sibling, def types.GovernanceUnit }

func teamChildUnit(parent types.GovernanceUnit, name string) types.GovernanceUnit {
	return types.GovernanceUnit{ID: uuid.New(), Name: name, Kind: types.UnitKindUnit, ParentID: &parent.ID,
		Overlay: &types.CeilingOverlay{}, Mode: types.UnitModePassThrough}
}

// teamDriveSite is the member drive switch at three a person, with the platform and the member team switches.
func teamDriveSite(platform, member bool) types.SiteConfig {
	s := memberDriveSite(3, 0)
	s.WorkspaceProviders.Storage.UserDrive.TeamDrives = platform
	s.WorkspaceProviders.Storage.UserDrive.MemberCreate.TeamDrives = member
	return s
}

func newTeamDriveStore() (*teamDriveStore, teamTree) {
	p := types.GovernanceUnit{ID: uuid.New(), Name: "Engineering", Kind: types.UnitKindUnit}
	u := teamChildUnit(p, "Platform")
	tr := teamTree{parent: p, unit: u, child: teamChildUnit(u, "Platform SRE"), sibling: teamChildUnit(p, "Data"),
		def: types.GovernanceUnit{ID: store.DefaultUnitID, Name: "Default", Kind: types.UnitKindDefault}}
	st := &teamDriveStore{memberDriveStore: newMemberDriveStore(), team: map[uuid.UUID]*teamFacts{},
		units: []types.GovernanceUnit{tr.def, tr.parent, tr.unit, tr.child, tr.sibling},
		unitOf: map[string]uuid.UUID{aliceSub: tr.unit.ID, carolSub: tr.child.ID, daveSub: tr.sibling.ID,
			erinSub: tr.parent.ID, frankSub: tr.def.ID}}
	st.site = teamDriveSite(true, true)
	return st, tr
}

func (s *teamDriveStore) unitByID(id uuid.UUID) (types.GovernanceUnit, bool) {
	i := slices.IndexFunc(s.units, func(u types.GovernanceUnit) bool { return u.ID == id })
	if i < 0 {
		return types.GovernanceUnit{}, false
	}
	return s.units[i], true
}

func (s *teamDriveStore) ResolveGovernanceUnit(_ context.Context, users, _ []string, _ string) (*types.GovernanceUnit, types.CapabilitySubjectType, error) {
	for _, sub := range users {
		if id, ok := s.unitOf[sub]; ok {
			if u, ok := s.unitByID(id); ok {
				return &u, types.CapabilitySubjectUser, nil
			}
		}
	}
	return nil, "", store.ErrNotFound
}

func (s *teamDriveStore) ListMembershipsFor(ctx context.Context, users, groups []string, userType string) ([]store.MembershipMatch, error) {
	return membershipsFromResolve(s.ResolveGovernanceUnit(ctx, users, groups, userType))
}

func (s *teamDriveStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	var chain []types.GovernanceUnit
	for next := &id; next != nil; {
		u, ok := s.unitByID(*next)
		if !ok {
			return nil, store.ErrNotFound
		}
		chain, next = append(chain, u), u.ParentID
	}
	return chain, nil
}

func (s *teamDriveStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	return s.units, nil
}

func (s *teamDriveStore) teamRow(id uuid.UUID) (store.MemberTeamDrive, bool) {
	d, ok := s.drives[id]
	f := s.team[id]
	if !ok || !d.Team || d.OwnerSubject == "" || f == nil {
		return store.MemberTeamDrive{}, false
	}
	return store.MemberTeamDrive{Drive: d, UnitID: f.unit, MembersCanWrite: f.write}, true
}

func (s *teamDriveStore) CreateMemberTeamDrive(ctx context.Context, d types.UserDrive, unit uuid.UUID, limit int) (types.UserDrive, error) {
	out, err := s.CreateMemberDrive(ctx, d, limit)
	if err != nil {
		return types.UserDrive{}, err
	}
	out.Team = true
	s.drives[out.ID], s.team[out.ID] = out, &teamFacts{unit: &unit}
	return out, nil
}

func (s *teamDriveStore) ListReachableMemberTeamDrives(_ context.Context, owner string, units []uuid.UUID) ([]store.MemberTeamDrive, error) {
	var out []store.MemberTeamDrive
	for id := range s.drives {
		t, ok := s.teamRow(id)
		if !ok || t.Drive.Deleting() || !(t.Drive.OwnerSubject == owner || t.UnitID != nil && slices.Contains(units, *t.UnitID)) {
			continue
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b store.MemberTeamDrive) int { return strings.Compare(a.Drive.Name, b.Drive.Name) })
	return out, nil
}

func (s *teamDriveStore) GetMemberTeamDrive(_ context.Context, id uuid.UUID) (store.MemberTeamDrive, error) {
	if t, ok := s.teamRow(id); ok {
		return t, nil
	}
	return store.MemberTeamDrive{}, store.ErrNotFound
}

func (s *teamDriveStore) AnyRunStarting(context.Context, time.Duration) (bool, error) {
	for _, on := range s.starting {
		if on {
			return true, nil
		}
	}
	return s.anyStarting, nil
}

func (s *teamDriveStore) MemberTeamDriveFacts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]store.MemberTeamDrive, error) {
	out := map[uuid.UUID]store.MemberTeamDrive{}
	for _, id := range ids {
		if t, ok := s.teamRow(id); ok {
			out[id] = t
		}
	}
	return out, nil
}

func (s *teamDriveStore) SetMemberTeamDriveWrite(_ context.Context, id uuid.UUID, on bool) (before, after store.MemberTeamDrive, err error) {
	before, ok := s.teamRow(id)
	if !ok {
		return before, after, store.ErrNotFound
	}
	if before.Drive.Deleting() {
		return store.MemberTeamDrive{}, store.MemberTeamDrive{}, store.ErrMemberDriveBusy
	}
	s.team[id].write = on
	after, _ = s.teamRow(id)
	return before, after, nil
}

// seedTeam is owner's own team drive of unit, writable, as a create leaves it.
func (s *teamDriveStore) seedTeam(owner, name string, unit uuid.UUID) types.UserDrive {
	d := seedMemberDrive(s.memberDriveStore, owner, name)
	d.Team = true
	s.drives[d.ID], s.team[d.ID] = d, &teamFacts{unit: &unit}
	return d
}

func teamDriveServer(st *teamDriveStore, target string) (*Server, *recRecorder, *memberDriveRunner) {
	rn := catalogueRunner()
	rn.outcome = runner.DriveReclaimDeleted
	rec := &recRecorder{}
	return New(Config{BaseCtx: context.Background(), Store: st, Audit: rec, Runner: rn, RunnerTarget: target}), rec, rn
}

func createTeamDrive(srv *Server, ctx context.Context, name, extra string) *httptest.ResponseRecorder {
	return memberCall(ctx, srv.handleCreateMemberDrive, http.MethodPost, "/api/v1/me/drives",
		`{"name":`+memberDriveJSON(name)+`,"team":true`+extra+`}`, nil)
}

// teamCatalogueRow is the drive id's row in sub's catalogue, or nil.
func teamCatalogueRow(t *testing.T, srv *Server, sub string, id uuid.UUID) *client.MeDrive {
	t.Helper()
	for _, d := range meDrivesOf(t, srv, personCtx(sub)).Drives {
		if d.ID == id.String() {
			return &d
		}
	}
	return nil
}

// TestMemberTeamDriveCreate_Gates: a team drive needs both switches, Docker, no Mac, and a unit that is not Default;
// each refusal has its own reason and sentence, and none leaves a row.
func TestMemberTeamDriveCreate_Gates(t *testing.T) {
	for _, tc := range []struct {
		name, sub, target, extra string
		site                     types.SiteConfig
		reason, msg              string
	}{
		{"platform switch off", aliceSub, "docker", "", teamDriveSite(false, true), reasonMemberTeamDrivesDisabled, memberTeamDrivesDisabledMsg()},
		{"member switch off", aliceSub, "docker", "", teamDriveSite(true, false), reasonMemberTeamDrivesDisabled, memberTeamDrivesDisabledMsg()},
		{"k8s deployment", aliceSub, "k8s", "", func() types.SiteConfig {
			s := teamDriveSite(true, true)
			s.WorkspaceProviders.Storage.UserDrive.MemberCreate.SizeMiB = 1024
			return s
		}(), reasonMemberTeamDriveUnsupported, memberTeamDriveUnsupportedMsg()},
		{"a Mac", aliceSub, "docker", `,"runner_id":"` + uuid.NewString() + `"`, teamDriveSite(true, true), reasonMemberTeamDriveUnsupported, memberTeamDriveMacMsg()},
		{"no unit", bobSub, "docker", "", teamDriveSite(true, true), reasonMemberTeamDriveNoUnit, memberTeamDriveNoUnitMsg()},
		{"Default", frankSub, "docker", "", teamDriveSite(true, true), reasonMemberTeamDriveNoUnit, memberTeamDriveNoUnitMsg()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newTeamDriveStore()
			st.site = tc.site
			srv, _, _ := teamDriveServer(st, tc.target)
			w := createTeamDrive(srv, personCtx(tc.sub), "Design assets", tc.extra)
			if body := wantReason(t, w, http.StatusUnprocessableEntity, tc.reason); body.Error != tc.msg {
				t.Errorf("sentence = %q, want %q", body.Error, tc.msg)
			}
			if n := len(st.owned("")); n != 0 {
				t.Errorf("a refused team drive left %d rows", n)
			}
		})
	}
	st, tr := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	w := createTeamDrive(srv, personCtx(aliceSub), "Design assets", "")
	if w.Code != http.StatusCreated {
		t.Fatalf("all doors open: %d %s", w.Code, w.Body.String())
	}
	mine := st.owned(aliceSub)
	if len(mine) != 1 || !mine[0].Team || st.team[mine[0].ID] == nil || *st.team[mine[0].ID].unit != tr.unit.ID {
		t.Fatalf("created %+v, want one team drive of %s", mine, tr.unit.Name)
	}
}

// TestMemberTeamDriveReach_UnitAndBelowOnly: the owner, and people in the drive's unit or a unit below it, reach the
// drive; a sibling, a parent, Default and a person in no unit do not.
func TestMemberTeamDriveReach_UnitAndBelowOnly(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	for sub, want := range map[string]bool{aliceSub: true, carolSub: true, daveSub: false, erinSub: false, frankSub: false, bobSub: false} {
		row := teamCatalogueRow(t, srv, sub, d.ID)
		if (row != nil) != want {
			t.Errorf("%s reaches the drive = %v, want %v", sub, row != nil, want)
		}
		if row != nil && (!row.Team || !row.SharedWithUnit || row.Source != client.MeDriveSourceTeam || row.Owned != (sub == aliceSub)) {
			t.Errorf("%s's row = %+v, want a team row shared with the unit, owned only by alice", sub, row)
		}
		_, f := authorizeDrives(srv, personCtx(sub), drivesRequest(d.ID.String()))
		if (f == nil) != want {
			t.Errorf("%s attaching it: refusal %+v, want attached %v", sub, f, want)
		}
	}
}

// TestMemberTeamDriveReach_ReadOnlyUntilTicked: the unit reads the drive and never writes it until a super admin
// ticks members can write; the owner writes either way; a read-only drive never becomes writable; and a run asking
// to write a read-only one is refused as a team drive's is.
func TestMemberTeamDriveReach_ReadOnlyUntilTicked(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	if row := teamCatalogueRow(t, srv, carolSub, d.ID); row == nil || row.Writable {
		t.Fatalf("carol's row before the tick = %+v, want read-only", row)
	}
	if row := teamCatalogueRow(t, srv, aliceSub, d.ID); row == nil || !row.Writable {
		t.Fatalf("alice's row = %+v, want writable", row)
	}
	no := false
	req := drivesRequest(d.ID.String())
	req.Drives[0].ReadOnly = &no
	drives, f := authorizeDrives(srv, personCtx(carolSub), req)
	if f == nil {
		w := httptest.NewRecorder()
		if _, ok := srv.driveMountsAt(w, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(personCtx(carolSub)), drives, drivePlace{}); ok ||
			!strings.Contains(w.Body.String(), driveRefusalReadOnly) {
			t.Errorf("carol asking to write it = %d %s, want the read_only refusal", w.Code, w.Body.String())
		}
	} else if f.body.Reason != driveRefusalReadOnly {
		t.Errorf("carol asking to write it = %+v, want the read_only refusal", f)
	}
	st.team[d.ID].write = true
	if row := teamCatalogueRow(t, srv, carolSub, d.ID); row == nil || !row.Writable {
		t.Errorf("carol's row after the tick = %+v, want writable", row)
	}
	ro := st.drives[d.ID]
	ro.Writable = false
	st.drives[d.ID] = ro
	if row := teamCatalogueRow(t, srv, carolSub, d.ID); row == nil || row.Writable {
		t.Errorf("a read-only drive with the tick = %+v, want read-only", row)
	}
}

// TestMemberTeamDriveReach_TeamSwitchOffOwnerOnly: with storage.user_drive.team_drives off, no one but the owner sees
// the drive, and the owner sees it unmountable.
func TestMemberTeamDriveReach_TeamSwitchOffOwnerOnly(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	st.site = teamDriveSite(false, true)
	if row := teamCatalogueRow(t, srv, carolSub, d.ID); row != nil {
		t.Errorf("carol reaches it with team drives off: %+v", row)
	}
	row := teamCatalogueRow(t, srv, aliceSub, d.ID)
	if row == nil || row.UnavailableReason != client.MeDriveUnavailableUnmountable {
		t.Errorf("alice's row = %+v, want unmountable", row)
	}
	if _, f := authorizeDrives(srv, personCtx(aliceSub), drivesRequest(d.ID.String())); f == nil {
		t.Error("alice attached her team drive with team drives off")
	}
}

// TestMemberTeamDriveCatalogue_DeletedUnitNotSharedWithUnit: a team drive whose unit was deleted is its owner's alone,
// so the owner's row is still a team drive but not shared with a unit; with its unit it is.
func TestMemberTeamDriveCatalogue_DeletedUnitNotSharedWithUnit(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	if row := teamCatalogueRow(t, srv, aliceSub, d.ID); row == nil || !row.Team || !row.SharedWithUnit {
		t.Fatalf("alice's row with its unit = %+v, want a team drive shared with the unit", row)
	}
	st.team[d.ID].unit = nil
	if row := teamCatalogueRow(t, srv, aliceSub, d.ID); row == nil || !row.Team || row.SharedWithUnit {
		t.Errorf("alice's row after the unit's delete = %+v, want a team drive not shared with a unit", row)
	}
	if row := teamCatalogueRow(t, srv, carolSub, d.ID); row != nil {
		t.Errorf("carol reaches it after the unit's delete: %+v", row)
	}
}

// TestMemberTeamDriveReach_PlainMemberDriveStaysOwnerOnly: a member drive without team reaches its owner only, even
// for a person in the owner's unit.
func TestMemberTeamDriveReach_PlainMemberDriveStaysOwnerOnly(t *testing.T) {
	st, _ := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	d := seedMemberDrive(st.memberDriveStore, aliceSub, "Notes")
	if row := teamCatalogueRow(t, srv, aliceSub, d.ID); row == nil || row.Team || row.Source != client.MeDriveSourcePersonal {
		t.Errorf("alice's row = %+v, want her personal drive", row)
	}
	for _, sub := range []string{carolSub, daveSub, erinSub} {
		if row := teamCatalogueRow(t, srv, sub, d.ID); row != nil {
			t.Errorf("%s reaches alice's plain drive: %+v", sub, row)
		}
		if _, f := authorizeDrives(srv, personCtx(sub), drivesRequest(d.ID.String())); f == nil || f.body.Reason != string(authz.ReasonDriveNotFound) {
			t.Errorf("%s attaching alice's plain drive = %+v, want drive_not_found", sub, f)
		}
	}
}

// TestMemberTeamDriveDispatch_RereadUnderFrozenUnit: dispatch's re-read (memberDrivesStillThere) of another person's
// team drive under the run's frozen unit: present while the run's unit is the drive's or below it; a run whose unit
// no longer has it, a plain member drive, team drives off, a legacy-held drive, a drive whose unit was deleted, a
// deleting drive and a lost row each fail the re-read. A legacy-held team drive is refused at attach too.
func TestMemberTeamDriveDispatch_RereadUnderFrozenUnit(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	plain := seedMemberDrive(st.memberDriveStore, aliceSub, "Notes")
	ctx := context.Background()
	reread := func(unit *uuid.UUID, id uuid.UUID) bool {
		return srv.memberDrivesStillThere(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: carolSub, UnitID: unit}, map[uuid.UUID]string{id: aliceSub})
	}
	for name, unit := range map[string]*uuid.UUID{"child": &tr.child.ID, "own unit": &tr.unit.ID} {
		if !reread(unit, d.ID) {
			t.Errorf("%s: a run whose unit has the drive failed the re-read", name)
		}
	}
	for name, unit := range map[string]*uuid.UUID{"sibling": &tr.sibling.ID, "parent": &tr.parent.ID, "no unit": nil} {
		if reread(unit, d.ID) {
			t.Errorf("%s: a run whose unit does not have the drive passed the re-read", name)
		}
	}
	if reread(&tr.child.ID, plain.ID) {
		t.Error("a plain member drive passed another person's re-read")
	}
	st.site = teamDriveSite(false, true)
	if reread(&tr.child.ID, d.ID) {
		t.Error("the re-read passed with team drives off")
	}
	st.site = teamDriveSite(true, true)
	held := st.drives[d.ID]
	held.LegacyUnsettled = true
	st.drives[d.ID] = held
	if reread(&tr.child.ID, d.ID) {
		t.Error("a legacy-held drive passed the re-read")
	}
	if _, f := authorizeDrives(srv, personCtx(carolSub), drivesRequest(d.ID.String())); f == nil || f.body.Reason != string(authz.ReasonDriveNotFound) {
		t.Errorf("carol attaching a legacy-held team drive = %+v, want drive_not_found", f)
	}
	held.LegacyUnsettled = false
	st.drives[d.ID] = held
	st.team[d.ID].unit = nil
	if reread(&tr.child.ID, d.ID) {
		t.Error("a drive whose unit was deleted passed the re-read")
	}
	st.team[d.ID].unit = &tr.unit.ID
	at := time.Now()
	tomb := st.drives[d.ID]
	tomb.DeleteStartedAt = &at
	st.drives[d.ID] = tomb
	if reread(&tr.child.ID, d.ID) {
		t.Error("a drive being deleted passed the re-read")
	}
	delete(st.drives, d.ID)
	if reread(&tr.child.ID, d.ID) {
		t.Error("a lost row passed the re-read")
	}
}

// TestMemberTeamDriveDispatch_OwnerRunNeedsTeamSwitch: the owner's own run re-reads their team drive under the
// platform switch too: present with team drives on, failed with them off; a plain member drive ignores the switch.
func TestMemberTeamDriveDispatch_OwnerRunNeedsTeamSwitch(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	plain := seedMemberDrive(st.memberDriveStore, aliceSub, "Notes")
	run := func() types.AgentRun { return types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub, UnitID: &tr.unit.ID} }
	ctx := context.Background()
	if !srv.memberDrivesStillThere(ctx, run(), map[uuid.UUID]string{d.ID: aliceSub}) {
		t.Fatal("the owner's run failed the re-read of their team drive with team drives on")
	}
	st.site = teamDriveSite(false, true)
	if srv.memberDrivesStillThere(ctx, run(), map[uuid.UUID]string{d.ID: aliceSub}) || st.failedRuns != 1 {
		t.Errorf("the owner's run passed the re-read of their team drive with team drives off (runs failed %d)", st.failedRuns)
	}
	if !srv.memberDrivesStillThere(ctx, run(), map[uuid.UUID]string{plain.ID: aliceSub}) {
		t.Error("the owner's plain member drive failed the re-read with team drives off")
	}
}

// TestMemberTeamDriveDelete_ReclaimsTheTeamObject: the delete destroys the object the unit's runs mount, the shared
// team folder, never a per-person name, and labels it with no person.
func TestMemberTeamDriveDelete_ReclaimsTheTeamObject(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, rn := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	drives, f := authorizeDrives(srv, personCtx(carolSub), drivesRequest(d.ID.String()))
	if f != nil || len(drives) != 1 {
		t.Fatalf("carol attaching it = %+v %+v", drives, f)
	}
	mounted := drives[0].resolved.ObjectName
	mount, err := memberDriveMount(d)
	if err != nil || mount.ObjectName != mounted || mount.HomeName != types.DriveTeamHome(d) || mount.SubjectHash != "" {
		t.Fatalf("delete's mount = %+v (%v), want the object runs mounted, %s, with no subject", mount, err, mounted)
	}
	if w := deleteMyDrive(srv, aliceSub, d.ID.String()); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	if len(rn.calls) != 1 || rn.calls[0].ObjectName != mounted {
		t.Fatalf("reclaim calls = %+v, want one of %s", rn.calls, mounted)
	}
	if _, kept := st.drives[d.ID]; kept {
		t.Error("the row stayed after a deleted outcome")
	}
}

// TestMemberTeamDriveMount_AuditsMountedBy: another person's mount of the drive writes run.drive.mount with team and
// mounted_by, read-only until the tick.
func TestMemberTeamDriveMount_AuditsMountedBy(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, rec, _ := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	ctx := personCtx(carolSub)
	drives, f := authorizeDrives(srv, ctx, drivesRequest(d.ID.String()))
	if f != nil {
		t.Fatalf("carol attaching it: %+v", f)
	}
	w := httptest.NewRecorder()
	mounts, ok := srv.driveMountsAt(w, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), drives, drivePlace{})
	if !ok || len(mounts) != 1 || !mounts[0].Team {
		t.Fatalf("mounts = %+v (%d %s), want one team mount", mounts, w.Code, w.Body.String())
	}
	srv.auditDriveMount(context.Background(), uuid.New(), carolSub, "", mounts)
	if data := driveAuditData(t, rec, "run.drive.mount"); data["team"] != true || data["mounted_by"] != carolSub || data["mode"] != "ro" ||
		data["drive_id"] != d.ID.String() {
		t.Errorf("mount row = %v, want team, mounted_by %s, ro", data, carolSub)
	}
}

// TestMemberTeamDriveErasure_Kept: a leaver's member_drives erasure keeps their team drive and says so, and still
// deletes their own drives.
func TestMemberTeamDriveErasure_Kept(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, rn := teamDriveServer(st, "docker")
	team := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	own := seedMemberDrive(st.memberDriveStore, aliceSub, "Notes")
	out, err := srv.eraseMemberDrivesOf(context.Background(), aliceSub)
	if err != nil {
		t.Fatalf("erasure: %v", err)
	}
	res, _ := out.(map[string]any)
	if res["kept_team_drives"] != 1 || res["drives"] != 1 {
		t.Errorf("result = %v, want one drive deleted and one team drive kept", res)
	}
	if _, kept := st.drives[team.ID]; !kept {
		t.Error("the team drive was deleted")
	}
	if _, kept := st.drives[own.ID]; kept || len(rn.calls) != 1 {
		t.Errorf("own drive kept %v, calls %d; want it deleted by one call", kept, len(rn.calls))
	}
}

// TestMemberTeamDriveErasure_FinishesAnAskedDelete: a team drive its owner had already asked to delete is not kept by
// the owner's erasure: the cleanup takes the stale claim over and deletes it.
func TestMemberTeamDriveErasure_FinishesAnAskedDelete(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, rn := teamDriveServer(st, "docker")
	team := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	at := st.now.Add(-2 * time.Minute)
	tomb := st.drives[team.ID]
	tomb.DeleteStartedAt = &at
	st.drives[team.ID] = tomb
	out, err := srv.eraseMemberDrivesOf(context.Background(), aliceSub)
	if err != nil {
		t.Fatalf("erasure: %v", err)
	}
	res, _ := out.(map[string]any)
	if res["kept_team_drives"] != 0 || res["drives"] != 1 {
		t.Errorf("result = %v, want the asked-for team drive deleted, none kept", res)
	}
	if _, kept := st.drives[team.ID]; kept || len(rn.calls) != 1 {
		t.Errorf("team drive kept %v, calls %d; want it deleted by one call", kept, len(rn.calls))
	}
}

// TestMemberTeamDriveCreate_AnswersAsTheCatalogueLists: the 201 body of a team drive's create carries team, source
// team and shared_with_unit, as the catalogue lists the same drive; a plain member drive's is personal and unshared.
func TestMemberTeamDriveCreate_AnswersAsTheCatalogueLists(t *testing.T) {
	st, _ := newTeamDriveStore()
	srv, _, _ := teamDriveServer(st, "docker")
	w := createTeamDrive(srv, personCtx(aliceSub), "Design assets", "")
	var got client.MeDrive
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	if !got.Team || got.Source != client.MeDriveSourceTeam || !got.SharedWithUnit {
		t.Errorf("201 body = %+v, want a team drive, source team, shared with the unit", got)
	}
	row := teamCatalogueRow(t, srv, aliceSub, uuid.MustParse(got.ID))
	if row == nil || row.Source != got.Source || row.SharedWithUnit != got.SharedWithUnit || row.Team != got.Team {
		t.Errorf("catalogue row = %+v, want what the 201 body said: %+v", row, got)
	}
	w = createMemberDrive(srv, personCtx(aliceSub), "Notes")
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("plain create = %d %s", w.Code, w.Body.String())
	}
	if got.Team || got.Source != client.MeDriveSourcePersonal || got.SharedWithUnit {
		t.Errorf("plain 201 body = %+v, want personal and unshared", got)
	}
}

// TestMemberTeamDriveCreate_AuditsTeamAndUnit: a team drive's drive.member.create row carries team and the unit; a
// plain member drive's carries neither.
func TestMemberTeamDriveCreate_AuditsTeamAndUnit(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, rec, _ := teamDriveServer(st, "docker")
	if w := createTeamDrive(srv, personCtx(aliceSub), "Design assets", ""); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	if data := driveAuditData(t, rec, "drive.member.create"); data["team"] != true || data["unit_id"] != tr.unit.ID.String() {
		t.Errorf("team create row = %v, want team and unit %s", data, tr.unit.ID)
	}
	if w := createMemberDrive(srv, personCtx(aliceSub), "Notes"); w.Code != http.StatusCreated {
		t.Fatalf("plain create = %d %s", w.Code, w.Body.String())
	}
	data := driveAuditData(t, rec, "drive.member.create")
	if _, ok := data["team"]; ok {
		t.Errorf("plain create row = %v, want no team", data)
	}
	if _, ok := data["unit_id"]; ok {
		t.Errorf("plain create row = %v, want no unit_id", data)
	}
}

// TestMemberTeamDriveDelete_PreTombstoneWaitsForAnyStartingRun: another person's STARTING run refuses a team drive's
// delete before the tombstone, writing no mark and making no call; a plain member drive still waits only for its
// owner's runs.
func TestMemberTeamDriveDelete_PreTombstoneWaitsForAnyStartingRun(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, rn := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	plain := seedMemberDrive(st.memberDriveStore, aliceSub, "Notes")
	st.starting = map[string]bool{carolSub: true}
	wantReason(t, deleteMyDrive(srv, aliceSub, d.ID.String()), http.StatusConflict, reasonMemberDriveInUse)
	if got := st.drives[d.ID]; got.DeleteStartedAt != nil || st.begins != 0 || len(rn.calls) != 0 {
		t.Fatalf("tombstone %v, tombstone writes %d, calls %d; want none", got.DeleteStartedAt, st.begins, len(rn.calls))
	}
	if w := deleteMyDrive(srv, aliceSub, plain.ID.String()); w.Code != http.StatusNoContent || len(rn.calls) != 1 {
		t.Fatalf("plain delete beside carol's starting run = %d (calls %d), want 204: %s", w.Code, len(rn.calls), w.Body.String())
	}
	st.starting = nil
	if w := deleteMyDrive(srv, aliceSub, d.ID.String()); w.Code != http.StatusNoContent || len(rn.calls) != 2 {
		t.Fatalf("team delete with no run starting = %d (calls %d), want 204: %s", w.Code, len(rn.calls), w.Body.String())
	}
}

// TestMemberTeamDriveDelete_PastTombstoneWaitsForAnyStartingRun: past its claim, a team drive's delete still waits
// for another person's STARTING run: refused in use, with no physical call and no issue recorded.
func TestMemberTeamDriveDelete_PastTombstoneWaitsForAnyStartingRun(t *testing.T) {
	st, tr := newTeamDriveStore()
	srv, _, rn := teamDriveServer(st, "docker")
	d := st.seedTeam(aliceSub, "Design assets", tr.unit.ID)
	ctx := withActor(context.Background(), types.ActorHuman, aliceSub)
	tomb, started, err := st.BeginMemberDriveDelete(ctx, aliceSub, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	st.starting = map[string]bool{carolSub: true}
	res := srv.deleteMemberDrive(ctx, st, tomb, started, memberDriveByOwner)
	if res.reason != reasonMemberDriveInUse || len(rn.calls) != 0 || st.drives[d.ID].DeleteIssues != 0 {
		t.Fatalf("delete = %+v, calls %d, issues %d; want in use, no call, no issue", res, len(rn.calls), st.drives[d.ID].DeleteIssues)
	}
}

// TestMemberTeamDriveCatalogue_TeamDrivesAllowed: the catalogue's team_drives_allowed is true exactly when the create
// gate would admit a team drive.
func TestMemberTeamDriveCatalogue_TeamDrivesAllowed(t *testing.T) {
	k8s := teamDriveSite(true, true)
	k8s.WorkspaceProviders.Storage.UserDrive.MemberCreate.SizeMiB = 1024
	limitOne := teamDriveSite(true, true)
	limitOne.WorkspaceProviders.Storage.UserDrive.MemberCreate.MaxPerPerson = 1
	for _, tc := range []struct {
		name, sub, target string
		site              types.SiteConfig
		seed              bool
		want              bool
	}{
		{"platform switch off", aliceSub, "docker", teamDriveSite(false, true), false, false},
		{"member switch off", aliceSub, "docker", teamDriveSite(true, false), false, false},
		{"both off", aliceSub, "docker", teamDriveSite(false, false), false, false},
		{"k8s deployment", aliceSub, "k8s", k8s, false, false},
		{"no unit", bobSub, "docker", teamDriveSite(true, true), false, false},
		{"Default", frankSub, "docker", teamDriveSite(true, true), false, false},
		{"plain create refused", aliceSub, "docker", limitOne, true, false},
		{"both on, Docker, a unit", aliceSub, "docker", teamDriveSite(true, true), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newTeamDriveStore()
			st.site = tc.site
			if tc.seed {
				seedMemberDrive(st.memberDriveStore, tc.sub, "Notes")
			}
			srv, _, _ := teamDriveServer(st, tc.target)
			got := meDrivesOf(t, srv, personCtx(tc.sub)).Create
			if got == nil || got.TeamDrivesAllowed != tc.want {
				t.Errorf("create = %+v, want team_drives_allowed %v", got, tc.want)
			}
			if got != nil && got.TeamDrivesAllowed && !got.Allowed {
				t.Errorf("team_drives_allowed without allowed: %+v", got)
			}
		})
	}
}
