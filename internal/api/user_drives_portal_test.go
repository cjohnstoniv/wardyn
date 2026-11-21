// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	// The same drive, allocated to the second portal too: its own allocation row, naming it and not the first portal.
	second := f.allocated
	second.Grant.ID, second.Grant.Subject = uuid.New(), f.two.String()
	f.st.rows[f.two] = []store.UserDriveWinner{second}
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
	if wantElsewhere := types.DrivePortalObjectName(f.allocated.Drive, types.DrivePortalHome(f.allocated.Drive, f.two)); elsewhere.resolved.ObjectName != wantElsewhere {
		t.Errorf("the second portal's folder = %q, want %q", elsewhere.resolved.ObjectName, wantElsewhere)
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
		if ref == nil || drives != nil || ref.decision == nil || ref.decision.Status != http.StatusForbidden ||
			ref.decision.Reason != authz.ReasonDriveNotAllocatedToPortal || ref.decision.Sentence != portalDriveNotAllocatedMsg {
			t.Errorf("%s = %+v, %+v; want 403 %s %q", name, drives, ref, authz.ReasonDriveNotAllocatedToPortal, portalDriveNotAllocatedMsg)
			continue
		}
		body, _ := json.Marshal(ref.decision)
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

// A row written around the admin routes (the database refuses two drives of one slug, so this is a resolver fixture, not
// a reachable state) that gives a portal two drives with one folder name is refused together at the run, 422
// drive_target_conflict, and the refusal is audited as every drive refusal is.
func TestPortalTeamDrive_TargetConflictRefusedAtRun(t *testing.T) {
	f := newPortalDriveFixture(t)
	second := portalAllocation("portal data", f.one, false) // the same slug as "Portal Data": one target
	f.st.rows[f.one] = []store.UserDriveWinner{f.allocated, second}
	ctx := viaPortal(driveMemberCtx(nil, false), f.one)
	drives, ref := authorizeDrives(f.srv, ctx, drivesRequest(f.allocated.Drive.ID.String(), second.Drive.ID.String()))
	if ref == nil || drives != nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(authz.ReasonDriveTargetConflict) {
		t.Errorf("two portal drives on one target = %+v, %+v; want 422 drive_target_conflict and no drives", drives, ref)
	}
	if drives, ref := authorizeDrives(f.srv, ctx, drivesRequest(f.allocated.Drive.ID.String())); ref != nil || len(drives) != 1 {
		t.Errorf("one of them alone = %+v, %+v; want it mounted", drives, ref)
	}
}

// A person who reaches a drive through no portal, or through a console portal, is answered exactly as before the
// portal rule: the same mounted drive for a group's allocation and the same 422 for an id that names nothing or
// another portal's drive, compared with a server whose store has no portal reads at all.
func TestPortalTeamDrive_GroupRequestAnsweredAsBefore(t *testing.T) {
	f := newPortalDriveFixture(t)
	plain, base := multiDriveServer(f.groupOnly)
	base.site = teamDrivesSite(true)
	answer := func(srv *Server, ctx context.Context, id string) string {
		drives, ref := authorizeDrives(srv, ctx, drivesRequest(id))
		out := map[string]any{"drives": []any{}}
		for _, d := range drives {
			out["drives"] = append(out["drives"].([]any), d.resolved)
			if d.portal {
				t.Errorf("a drive reached through no portal came back marked as a portal's: %+v", d.resolved)
			}
		}
		if ref != nil {
			out["status"], out["body"] = ref.status, ref.body
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for who, ctx := range map[string]context.Context{
		"a member through no portal":        driveMemberCtx([]string{"eng"}, false),
		"a member through a console portal": viaPortal(driveMemberCtx([]string{"eng"}, false), f.console),
	} {
		for what, id := range map[string]string{
			"the group's drive":        f.groupOnly.Drive.ID.String(),
			"an id that names nothing": uuid.NewString(),
			"another portal's drive":   f.otherPortal.Drive.ID.String(),
		} {
			want, got := answer(plain, driveMemberCtx([]string{"eng"}, false), id), answer(f.srv, ctx, id)
			if got != want {
				t.Errorf("%s asking for %s: %s, want what a server with no portal rule answers: %s", who, what, got, want)
			}
		}
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

// A profile with the user drive door shut refuses a portal's drives[] the same as anyone's: the door is judged
// before the portal's allocations are read, so an allocation cannot open it. With the door open the same request
// mounts the drive.
func TestPortalTeamDrive_UnitDenyUserDriveRefuses_PG(t *testing.T) {
	f := newPortalDriveFixture(t)
	ctx := viaPortal(driveMemberCtx([]string{"eng"}, false), f.one)
	req := drivesRequest(f.allocated.Drive.ID.String())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)

	drives, ref := f.srv.authorizeRequestDrives(r, req, deniedCeiling())
	if ref == nil || drives != nil || ref.decision == nil || ref.decision.Status != http.StatusForbidden || ref.decision.Reason != authz.ReasonGovernanceUnit {
		t.Fatalf("a shut door = %+v, %+v; want 403 %s", drives, ref, authz.ReasonGovernanceUnit)
	}
	if drives, ref := f.srv.authorizeRequestDrives(r, req, governanceCeiling{}); ref != nil || len(drives) != 1 || !drives[0].portal {
		t.Errorf("the open door = %+v, %+v; want the portal's drive", drives, ref)
	}
}

// The queued start's mode cap, value by value (input -> before -> after):
//
//	context          recorded mode  read_only asked  -> read_only mounted
//	not a queued run  -             nil               nil (the fresh resolution decides)
//	not a queued run  -             false             false
//	queued            read-only     nil               true
//	queued            read-only     false             true  (never widened)
//	queued            read-only     true              true
//	queued            writable      nil               nil   (the fresh resolution decides, and may narrow)
//	queued            writable      false             false
//	queued            writable      true              true
//	queued            none          nil               true  (no record: read-only)
func TestPortalTeamDrive_QueuedModeCapValues(t *testing.T) {
	yes, no := true, false
	const id = "d"
	for _, c := range []struct {
		name  string
		ctx   context.Context
		asked *bool
		want  *bool
	}{
		{"not queued, nil", context.Background(), nil, nil},
		{"not queued, false", context.Background(), &no, &no},
		{"read-only, nil", withPortalDriveModes(context.Background(), map[string]bool{id: true}), nil, &yes},
		{"read-only, false", withPortalDriveModes(context.Background(), map[string]bool{id: true}), &no, &yes},
		{"read-only, true", withPortalDriveModes(context.Background(), map[string]bool{id: true}), &yes, &yes},
		{"writable, nil", withPortalDriveModes(context.Background(), map[string]bool{id: false}), nil, nil},
		{"writable, false", withPortalDriveModes(context.Background(), map[string]bool{id: false}), &no, &no},
		{"writable, true", withPortalDriveModes(context.Background(), map[string]bool{id: false}), &yes, &yes},
		{"no record, nil", withPortalDriveModes(context.Background(), nil), nil, &yes},
		{"another drive recorded, nil", withPortalDriveModes(context.Background(), map[string]bool{"other": false}), nil, &yes},
	} {
		got := portalDriveModeCap(c.ctx, id, c.asked)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: cap = %v, want %v", c.name, got, c.want)
		}
	}
}

// Through the real authorization: a portal drive whose allocation is writable now mounts read-only when the queued
// run was admitted read-only or with no record, mounts writable when admitted writable, and mounts read-only when the
// allocation has become read-only whatever it was admitted with.
func TestPortalTeamDrive_QueuedStartNeverWidensTheMode(t *testing.T) {
	for _, c := range []struct {
		name         string
		nowWritable  bool
		modes        map[string]bool // nil: not a queued start
		queued       bool
		wantReadOnly bool
	}{
		{"admitted read-only, now writable", true, map[string]bool{}, true, true},
		{"admitted writable, still writable", true, map[string]bool{}, true, false},
		{"admitted writable, now read-only", false, map[string]bool{}, true, true},
		{"no record, now writable", true, nil, true, true},
		{"an ordinary create, writable", true, nil, false, false},
	} {
		f := newPortalDriveFixture(t)
		f.allocated.Grant.MembersCanWrite = c.nowWritable
		f.st.rows[f.one] = []store.UserDriveWinner{f.allocated}
		id := f.allocated.Drive.ID.String()
		modes := c.modes
		switch c.name {
		case "admitted read-only, now writable":
			modes = map[string]bool{id: true}
		case "admitted writable, still writable", "admitted writable, now read-only":
			modes = map[string]bool{id: false}
		}
		ctx := viaPortal(driveMemberCtx(nil, false), f.one)
		if c.queued {
			ctx = withPortalDriveModes(ctx, modes)
		}
		drives, ref := authorizeDrives(f.srv, ctx, drivesRequest(id))
		if ref != nil || len(drives) != 1 {
			t.Errorf("%s: drives %+v refusal %+v, want one drive", c.name, drives, ref)
			continue
		}
		if got := drives[0].readOnly(); got != c.wantReadOnly {
			t.Errorf("%s: read-only = %v, want %v", c.name, got, c.wantReadOnly)
		}
	}
}

// A queued run's stored launch records the mode of each portal drive and no other.
func TestPortalTeamDrive_PortalDriveModesOfTheLaunch(t *testing.T) {
	ro := false
	drives := []runDrive{
		{id: "a", portal: true, narrow: &ro, resolved: types.ResolvedDrive{Writable: true}},
		{id: "b", portal: true, resolved: types.ResolvedDrive{Writable: false}},
		{id: "c", resolved: types.ResolvedDrive{Writable: true}},
	}
	got := portalDriveModes(drives)
	if len(got) != 2 || got["a"] != false || got["b"] != true {
		t.Errorf("modes = %v, want a:writable b:read-only and nothing for the group drive", got)
	}
	if portalDriveModes(drives[2:]) != nil {
		t.Error("a launch with no portal drive records modes")
	}
}

// A queued run that mounted a portal's drive ends portal_run_scope unless the stored authority names the portal that
// created it, whatever marks the authority carries. A run with no portal drive is never judged by it.
func TestQueuePortalRunScope_JudgedWhateverTheAuthorityMarks(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	marked := func(p uuid.UUID) queueAuthority {
		return queueAuthority{Delegate: &p, Grant: &queueGrant{Portal: true}}
	}
	unmarked := func(p uuid.UUID) queueAuthority { return queueAuthority{Delegate: &p} }
	for _, c := range []struct {
		label     string
		portal    bool
		createdBy *uuid.UUID
		auth      queueAuthority
		ends      bool
	}{
		{"portal drives, created by A, authority names A (marked)", true, &a, marked(a), false},
		{"portal drives, created by A, authority names B (marked)", true, &a, marked(b), true},
		{"portal drives, created by A, authority names B without a portal mark", true, &a, unmarked(b), true},
		{"portal drives, created by A, authority names A without a portal mark", true, &a, unmarked(a), false},
		{"portal drives, created by A, authority without a portal", true, &a, queueAuthority{}, true},
		{"portal drives, no creator", true, nil, marked(a), true},
		{"no portal drives, created by A, authority names B", false, &a, marked(b), false},
		{"no portal drives, authority without a portal", false, nil, queueAuthority{}, false},
	} {
		run := types.AgentRun{PortalDrives: c.portal, CreatedVia: c.createdBy}
		end := queuePortalRunScope(run, c.auth)
		if (end != nil) != c.ends || (end != nil && end.token != queueEndPortalRunScope) {
			t.Errorf("%s: end = %+v, want ends %v with %q", c.label, end, c.ends, queueEndPortalRunScope)
		}
	}
}
