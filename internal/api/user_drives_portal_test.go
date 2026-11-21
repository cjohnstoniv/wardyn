// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// portalDriveStore is multiDriveStore plus the portal reads: the portals' rows and each portal's allocations,
// answered the way the store does (a live issuer portal's own rows, nothing else).
type portalDriveStore struct {
	*multiDriveStore
	portals map[uuid.UUID]types.Delegate
	rows    map[uuid.UUID][]store.UserDriveWinner
}

func (s *portalDriveStore) GetDelegateByID(_ context.Context, id uuid.UUID) (types.Delegate, error) {
	d, ok := s.portals[id]
	if !ok {
		return types.Delegate{}, store.ErrNotFound
	}
	return d, nil
}

func (s *portalDriveStore) ResolvePortalTeamDrives(_ context.Context, portal uuid.UUID) ([]store.UserDriveWinner, error) {
	return s.rows[portal], nil
}

// portalAllocation is a team drive on docker_volume allocated to portal, read-only unless writable.
func portalAllocation(name string, portal uuid.UUID, writable bool) store.UserDriveWinner {
	w := winnerOf(name, types.DriveBackendDockerVolume, types.DriveSubjectPortal, portal.String(), true)
	w.Drive.Team = true
	w.Grant.MembersCanWrite = writable
	return w
}

type portalDriveFixture struct {
	srv         *Server
	st          *portalDriveStore
	one, two    uuid.UUID // two issuer portals
	console     uuid.UUID
	allocated   store.UserDriveWinner // allocated to one
	otherPortal store.UserDriveWinner // allocated to two
	groupOnly   store.UserDriveWinner // allocated to the person's group
}

func newPortalDriveFixture(t *testing.T) *portalDriveFixture {
	t.Helper()
	f := &portalDriveFixture{one: uuid.New(), two: uuid.New(), console: uuid.New()}
	f.allocated = portalAllocation("Portal Data", f.one, false)
	f.otherPortal = portalAllocation("Other Data", f.two, false)
	f.groupOnly = winnerOf("Group Data", types.DriveBackendDockerVolume, types.CapabilitySubjectGroup, "eng", false)
	f.groupOnly.Drive.Team = true
	_, base := multiDriveServer(f.groupOnly)
	base.site = teamDrivesSite(true)
	f.st = &portalDriveStore{multiDriveStore: base,
		portals: map[uuid.UUID]types.Delegate{
			f.one:     {ID: f.one, Issuer: "https://idp.example.com"},
			f.two:     {ID: f.two, Issuer: "https://idp.example.com"},
			f.console: {ID: f.console},
		},
		rows: map[uuid.UUID][]store.UserDriveWinner{f.one: {f.allocated}, f.two: {f.otherPortal}}}
	f.srv = New(Config{Store: f.st, Audit: &recRecorder{}, RunnerTarget: "docker"})
	return f
}

func viaPortal(ctx context.Context, portal uuid.UUID) context.Context {
	return audit.WithDelegation(ctx, types.DelegationVia{Delegate: portal, Grant: uuid.New()})
}

// A portal's run mounts the portal's own folder read-only, at one object for every person of the portal and a
// different one per portal and drive.
func TestPortalTeamDrive_SecondPersonSameFolder(t *testing.T) {
	f := newPortalDriveFixture(t)
	f.st.rows[f.two] = []store.UserDriveWinner{f.allocated} // the same drive, allocated to the second portal too
	reach := func(portal uuid.UUID, sub string) runDrive {
		ctx := viaPortal(operatorCtx(sub, sub+"@corp.example", "user"), portal)
		drives, ref := authorizeDrives(f.srv, ctx, drivesRequest(f.allocated.Drive.ID.String()))
		if ref != nil || len(drives) != 1 {
			t.Fatalf("portal %s person %s: drives %+v refusal %+v, want one drive", portal, sub, drives, ref)
		}
		return drives[0]
	}
	alice, bob, elsewhere := reach(f.one, "sub-alice"), reach(f.one, "sub-bob"), reach(f.two, "sub-alice")
	if !alice.portal || !alice.resolved.Team || alice.resolved.Writable || alice.resolved.SubjectHash != "" {
		t.Errorf("portal drive = %+v, want a portal, team, read-only folder with no subject hash", alice.resolved)
	}
	if alice.resolved.ObjectName != bob.resolved.ObjectName || alice.resolved.HomeName != bob.resolved.HomeName {
		t.Errorf("two people of one portal got %q and %q, want one folder", alice.resolved.ObjectName, bob.resolved.ObjectName)
	}
	if elsewhere.resolved.ObjectName == alice.resolved.ObjectName {
		t.Errorf("two portals share the folder %q, want one folder each", alice.resolved.ObjectName)
	}
	wantHome := types.DrivePortalHome(f.allocated.Drive, f.one)
	if alice.resolved.HomeName != wantHome || alice.resolved.ObjectName != types.DrivePortalObjectName(f.allocated.Drive, wantHome) {
		t.Errorf("home/object = %q/%q, want %q and its object", alice.resolved.HomeName, alice.resolved.ObjectName, wantHome)
	}
	if got := runner.DriveTargetFor(alice.resolved, false); got != runner.SharedRoot+"/portal-data" {
		t.Errorf("target = %q, want %s/portal-data", got, runner.SharedRoot)
	}
}

// Every portal drive that its allocations do not reach is one 403: another portal's, the person's group's, an
// unknown id, and the legacy drive flag, with one body and one audited reason. A member through no portal keeps
// 422 drive_not_found for the same unmatched id.
func TestPortalTeamDrive_UnallocatedOrOtherPortalIs403(t *testing.T) {
	f := newPortalDriveFixture(t)
	ctx := viaPortal(driveMemberCtx([]string{"eng"}, false), f.one)
	ask := map[string]createRunRequest{
		"another portal's drive":   drivesRequest(f.otherPortal.Drive.ID.String()),
		"a group's drive":          drivesRequest(f.groupOnly.Drive.ID.String()),
		"an id that names nothing": drivesRequest(uuid.NewString()),
		"the legacy drive flag":    {Agent: "claude-code", Task: "t", Drive: &client.DriveSelection{Enabled: true}},
	}
	var first string
	for name, req := range ask {
		drives, ref := authorizeDrives(f.srv, ctx, req)
		if ref == nil || drives != nil || ref.status != http.StatusForbidden || ref.body.Reason != string(authz.ReasonDriveNotAllocatedToPortal) ||
			ref.body.Error != portalDriveNotAllocatedMsg {
			t.Errorf("%s = %+v, %+v; want 403 %s %q", name, drives, ref, authz.ReasonDriveNotAllocatedToPortal, portalDriveNotAllocatedMsg)
			continue
		}
		body, _ := json.Marshal(ref.body)
		if first == "" {
			first = string(body)
		} else if string(body) != first {
			t.Errorf("%s answered a different body %s, want %s", name, body, first)
		}
	}
	for _, id := range []string{f.otherPortal.Drive.ID.String(), uuid.NewString()} {
		_, ref := authorizeDrives(f.srv, driveMemberCtx([]string{"eng"}, false), drivesRequest(id))
		if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(authz.ReasonDriveNotFound) {
			t.Errorf("a member (no portal) asking %s = %+v, want the unchanged 422 drive_not_found", id, ref)
		}
	}
	// A group's drive stays the person's own through no portal.
	if drives, ref := authorizeDrives(f.srv, driveMemberCtx([]string{"eng"}, false), drivesRequest(f.groupOnly.Drive.ID.String())); ref != nil || len(drives) != 1 || drives[0].portal {
		t.Errorf("a member's group drive = %+v, %+v; want it mounted, not as a portal's", drives, ref)
	}
}

// A console portal (no issuer) keeps the person's own catalogue: it reaches the group's drive and gets no portal
// answer.
func TestPortalTeamDrive_ConsolePortalKeepsTheMemberPath(t *testing.T) {
	f := newPortalDriveFixture(t)
	ctx := viaPortal(driveMemberCtx([]string{"eng"}, false), f.console)
	drives, ref := authorizeDrives(f.srv, ctx, drivesRequest(f.groupOnly.Drive.ID.String()))
	if ref != nil || len(drives) != 1 || drives[0].portal {
		t.Errorf("console portal = %+v, %+v; want the member's group drive, not a portal drive", drives, ref)
	}
}

// A portal row that cannot be read is a 503, never "not an issuer portal" (which would fall back to the person's
// catalogue).
func TestPortalTeamDrive_UnreadablePortalIsUnavailable(t *testing.T) {
	f := newPortalDriveFixture(t)
	ctx := viaPortal(driveMemberCtx([]string{"eng"}, false), uuid.New()) // not in the store
	drives, ref := authorizeDrives(f.srv, ctx, drivesRequest(f.groupOnly.Drive.ID.String()))
	if ref == nil || drives != nil || ref.status != http.StatusServiceUnavailable {
		t.Errorf("an unreadable portal = %+v, %+v; want 503", drives, ref)
	}
}

// A portal folder is read-only until the allocation says members_can_write, and a run asking to write is refused.
func TestPortalTeamDrive_WriteRefusedUnlessMembersCanWrite(t *testing.T) {
	d, g := teamDrive()
	portal := uuid.New()
	g.SubjectType, g.Subject = types.DriveSubjectPortal, portal.String()
	on := driveSizeCeiling{TeamDrives: true}
	ro, err := newResolvedDrive(&d, &g, types.DriveSubjectPortal, nil, on)
	if err != nil || ro.Writable {
		t.Fatalf("default portal allocation: %+v, %v; want read-only", ro, err)
	}
	no := false
	if driveReadOnlyRefusal(&no, *ro) == nil {
		t.Error("a run asking to write a read-only portal folder was not refused")
	}
	g.MembersCanWrite = true
	if rw, err := newResolvedDrive(&d, &g, types.DriveSubjectPortal, nil, on); err != nil || !rw.Writable {
		t.Errorf("members_can_write: %+v, %v; want writable", rw, err)
	}
}

// With the team switch off a portal's allocation mounts nothing: no per-person fallback.
func TestPortalTeamDrive_TeamSwitchOffMountsNothing(t *testing.T) {
	d, g := teamDrive()
	g.SubjectType, g.Subject = types.DriveSubjectPortal, uuid.NewString()
	_, err := newResolvedDrive(&d, &g, types.DriveSubjectPortal, nil, driveSizeCeiling{})
	if !errors.Is(err, errDriveUnmountable) || !strings.Contains(err.Error(), driveTeamOffMsg) {
		t.Errorf("switch off: err = %v, want errDriveUnmountable naming the switch", err)
	}
}

// A row written around the write boundary (a second place, a host path, a non-uuid subject) mounts nothing.
func TestPortalTeamDrive_SecondPlaceRefusedAtResolve(t *testing.T) {
	on := driveSizeCeiling{TeamDrives: true}
	for name, edit := range map[string]func(*types.UserDrive, *types.UserDriveGrant){
		"a second place": func(d *types.UserDrive, _ *types.UserDriveGrant) {
			d.AltBackend, d.AltHostRoot = types.DriveBackendHostPath, "/srv/x"
		},
		"a host path": func(d *types.UserDrive, _ *types.UserDriveGrant) {
			d.Backend, d.HostRoot = types.DriveBackendHostPath, "/srv/x"
		},
		"a non-uuid subject": func(_ *types.UserDrive, g *types.UserDriveGrant) { g.Subject = "platform" },
		"a home override":    func(_ *types.UserDrive, g *types.UserDriveGrant) { g.HomeOverride = "elsewhere" },
	} {
		d, g := teamDrive()
		g.SubjectType, g.Subject = types.DriveSubjectPortal, uuid.NewString()
		edit(&d, &g)
		if _, err := newResolvedDrive(&d, &g, types.DriveSubjectPortal, nil, on); !errors.Is(err, errDriveUnmountable) {
			t.Errorf("%s: err = %v, want errDriveUnmountable", name, err)
		}
	}
}

// A disabled allocation stays in the answer and resolves paused, and never falls through to the person's
// catalogue.
func TestPortalTeamDrive_PausedAllocationRefuses(t *testing.T) {
	f := newPortalDriveFixture(t)
	f.allocated.Grant.Enabled = false
	f.st.rows[f.one] = []store.UserDriveWinner{f.allocated}
	ctx := viaPortal(driveMemberCtx([]string{"eng"}, false), f.one)
	if drives, ref := authorizeDrives(f.srv, ctx, drivesRequest(f.allocated.Drive.ID.String())); ref == nil || drives != nil || ref.drive == nil || ref.drive.reason != driveRefusalPaused {
		t.Errorf("paused portal drive = %+v, %+v; want the paused refusal", drives, ref)
	}
}

// The run.drive.mount row of a portal's folder names the portal beside the person; a drive not in the map keeps
// today's row.
func TestPortalTeamDrive_MountAuditNamesPersonPortalDrive(t *testing.T) {
	portal, drive, own := uuid.New(), uuid.New(), uuid.New()
	rec := &recRecorder{}
	srv := New(Config{BaseCtx: testBaseCtx(t), Audit: rec, LocalMode: true})
	srv.auditDriveMount(context.Background(), uuid.New(), "sub-alice", "", []types.DriveMount{
		{DriveID: drive, Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-portal-x", Target: runner.SharedRoot + "/team", ReadOnly: true, Team: true},
		{DriveID: own, Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-drive-y", Target: runner.SharedRoot + "/team", ReadOnly: true, Team: true},
	}, map[uuid.UUID]uuid.UUID{drive: portal})
	if len(rec.events) != 2 {
		t.Fatalf("rows = %d, want 2", len(rec.events))
	}
	var via, plain map[string]any
	_ = json.Unmarshal(rec.events[0].Data, &via)
	_ = json.Unmarshal(rec.events[1].Data, &plain)
	if via["portal"] != portal.String() || via["mounted_by"] != "sub-alice" || via["drive_id"] != drive.String() {
		t.Errorf("portal row = %v, want portal %s, mounted_by sub-alice and the drive", via, portal)
	}
	if _, ok := plain["portal"]; ok {
		t.Errorf("a drive outside the map carries a portal: %v", plain)
	}
}

// portalDriveMounts and runMountsPortalDrive read only the drives that came through the portal.
func TestPortalTeamDrive_RunFlagFollowsThePortalDrives(t *testing.T) {
	via := uuid.New()
	a, b := uuid.New(), uuid.New()
	drives := []runDrive{{portal: true, resolved: types.ResolvedDrive{Drive: types.UserDrive{ID: a}}}, {resolved: types.ResolvedDrive{Drive: types.UserDrive{ID: b}}}}
	if got := portalDriveMounts(drives, &via); len(got) != 1 || got[a] != via {
		t.Errorf("portalDriveMounts = %v, want only %s", got, a)
	}
	if !runMountsPortalDrive(drives) || runMountsPortalDrive(drives[1:]) {
		t.Error("runMountsPortalDrive does not follow the portal drives")
	}
	if got := portalDriveMounts(drives, nil); got != nil {
		t.Errorf("portalDriveMounts with no portal = %v, want nil", got)
	}
}

// A run that mounted portal one's drive is reachable by id and in lists through portal one only: portal two on
// the same issuer, which serves the same person, is 403 portal_run_scope, whatever the pin.
func TestPortalTeamDrive_OtherPortalCannotEnterDriveRun(t *testing.T) {
	f := newPinFixture(t)
	seed := func(via uuid.UUID, portalDrives bool) uuid.UUID {
		id := uuid.New()
		f.ast.mu.Lock()
		f.ast.runs[id] = types.AgentRun{ID: id, CreatedBy: f.person, State: types.RunRunning, Agent: "claude-code", UnitID: &f.child.ID,
			CreatedVia: &via, PortalDrives: portalDrives}
		f.ast.mu.Unlock()
		return id
	}
	mine := seed(f.delegate.ID, true)
	theirs := seed(uuid.New(), true)
	plainOther := seed(uuid.New(), false)
	for _, route := range []string{"GET /api/v1/runs/{id}", "POST /api/v1/runs/{id}/kill"} {
		if code, reason, body := f.request(t, route, theirs); code != http.StatusForbidden || reason != "portal_run_scope" {
			t.Errorf("%s of a run that mounted another portal's drive = %d %q %s, want 403 portal_run_scope", route, code, reason, body)
		}
	}
	if code, reason, _ := f.request(t, "GET /api/v1/runs/{id}", mine); code != http.StatusOK {
		t.Errorf("own portal's drive run = %d %q, want 200", code, reason)
	}
	if code, reason, _ := f.request(t, "GET /api/v1/runs/{id}", plainOther); code != http.StatusOK {
		t.Errorf("a run with no portal drive from another portal = %d %q, want today's 200", code, reason)
	}
}

// A person's reclaim is refused for a team drive a portal holds, the same as for a group's: the folder belongs to
// the portal, not to any person, and the refusal reads the drive's team flag, never its allocations.
func TestPortalTeamDrive_ReclaimRefused(t *testing.T) {
	f := newPortalDriveFixture(t)
	f.st.site = teamDrivesSite(true)
	code, msg := f.srv.driveTeamReclaimRefusal(context.Background(), f.allocated.Drive)
	if code != http.StatusUnprocessableEntity || msg != driveTeamReclaimMsg {
		t.Errorf("reclaim of a portal's team drive = %d %q, want 422 %q", code, msg, driveTeamReclaimMsg)
	}
}
