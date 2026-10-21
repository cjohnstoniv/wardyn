// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// multiDriveStore answers ResolveUserDrives the way the store does: one winner
// per drive, already in precedence order, filtered to the subjects asked with
// (a nil group list is the stale branch's ungrouped read).
type multiDriveStore struct {
	*driveStore
	winners []store.UserDriveWinner
}

func (s *multiDriveStore) ResolveUserDrives(_ context.Context, users, groups []string, _ string) ([]store.UserDriveWinner, error) {
	var out []store.UserDriveWinner
	for _, w := range s.winners {
		switch w.Grant.SubjectType {
		case types.CapabilitySubjectUser:
			if !slices.Contains(users, w.Grant.Subject) {
				continue
			}
		case types.CapabilitySubjectGroup:
			if !slices.Contains(groups, w.Grant.Subject) {
				continue
			}
		}
		out = append(out, w)
	}
	return out, nil
}

// winnerOf is one drive and the grant that wins it for the test member.
func winnerOf(name string, backend types.DriveBackend, tier types.CapabilitySubjectType, subject string, writable bool) store.UserDriveWinner {
	d := types.UserDrive{ID: uuid.New(), Name: name, Backend: backend, HomeTemplate: types.HomeTemplateHash,
		SizeMiB: 1024, Writable: writable, Reclaim: types.DriveReclaimRetain}
	if backend.Kind() == types.DriveKindShare {
		d.HomeTemplate, d.HostRoot = types.HomeTemplateSub, "/srv/share"
	}
	return store.UserDriveWinner{Drive: d, Grant: types.UserDriveGrant{ID: uuid.New(), SubjectType: tier, Subject: subject, DriveID: d.ID, Enabled: true}}
}

// The member's catalogue: their own drive (legacy), a team drive, an
// organisation share, and a drive granted to somebody else.
type driveSet struct {
	personal, team, share, foreign store.UserDriveWinner
}

func newDriveSet() driveSet {
	return driveSet{
		personal: winnerOf("Personal", types.DriveBackendDockerVolume, types.CapabilitySubjectUser, "sub-drive-bob", true),
		team:     winnerOf("Team Data", types.DriveBackendDockerVolume, types.CapabilitySubjectGroup, "eng", false),
		share:    winnerOf("Shared Docs", types.DriveBackendHostPath, types.CapabilitySubjectAll, "", true),
		foreign:  winnerOf("Theirs", types.DriveBackendDockerVolume, types.CapabilitySubjectUser, "sub-other", true),
	}
}

func multiDriveServer(ws ...store.UserDriveWinner) (*Server, *multiDriveStore) {
	st := &multiDriveStore{driveStore: &driveStore{}, winners: ws}
	return New(Config{Store: st, Audit: &recRecorder{}, RunnerTarget: "docker"}), st
}

func drivesRequest(ids ...string) createRunRequest {
	req := createRunRequest{Agent: "claude-code", Task: "t"}
	for _, id := range ids {
		req.Drives = append(req.Drives, client.DriveAttachment{DriveID: id})
	}
	return req
}

func authorizeDrives(srv *Server, ctx context.Context, req createRunRequest) ([]runDrive, *runRefusal) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	return srv.authorizeRequestDrives(r, req, governanceCeiling{})
}

func TestDriveSelectionRefusal(t *testing.T) {
	id := uuid.NewString()
	many := drivesRequest()
	for range maxRunDrives + 1 {
		many.Drives = append(many.Drives, client.DriveAttachment{DriveID: uuid.NewString()})
	}
	both := drivesRequest(id)
	both.Drive = &client.DriveSelection{Enabled: true}
	for name, c := range map[string]struct {
		req    createRunRequest
		reason string
		msg    string
	}{
		"drive and drives":    {both, reasonDriveSelectionConflict, driveSelectionConflictMsg()},
		"not a uuid":          {drivesRequest("team"), reasonDriveInvalid, driveInvalidMsg(0)},
		"not canonical":       {drivesRequest(strings.ToUpper(id)), reasonDriveInvalid, driveInvalidMsg(0)},
		"the nil uuid":        {drivesRequest(uuid.Nil.String()), reasonDriveInvalid, driveInvalidMsg(0)},
		"listed twice":        {drivesRequest(id, id), reasonDriveInvalid, driveInvalidMsg(1)},
		"more than the bound": {many, reasonDriveInvalid, driveTooManyMsg(maxRunDrives)},
	} {
		t.Run(name, func(t *testing.T) {
			ref := driveSelectionRefusal(c.req)
			if ref == nil || ref.status != http.StatusBadRequest || ref.body.Reason != c.reason || ref.body.Error != c.msg {
				t.Fatalf("refusal = %+v, want 400 %s %q", ref, c.reason, c.msg)
			}
		})
	}
	if ref := driveSelectionRefusal(drivesRequest(id, uuid.NewString())); ref != nil {
		t.Errorf("two drives = %+v, want accepted", ref)
	}
}

// TestAuthorizeRequestDrives_TargetsAndLegacy: each attachment resolves through
// its own grant and mounts at its derived path; the caller's top-ranked drive
// keeps /home/agent/drive.
func TestAuthorizeRequestDrives_TargetsAndLegacy(t *testing.T) {
	ds := newDriveSet()
	srv, _ := multiDriveServer(ds.personal, ds.team, ds.share, ds.foreign)
	drives, ref := authorizeDrives(srv, driveMemberCtx([]string{"eng"}, false),
		drivesRequest(ds.team.Drive.ID.String(), ds.personal.Drive.ID.String(), ds.share.Drive.ID.String()))
	if ref != nil {
		t.Fatalf("refused: %+v", ref)
	}
	want := map[string]string{
		"Team Data":   runner.DrivesRoot + "/team-data",
		"Personal":    runner.DriveTarget,
		"Shared Docs": runner.SharedRoot + "/shared-docs",
	}
	for _, d := range drives {
		if d.target != want[d.resolved.Drive.Name] || d.legacy != (d.resolved.Drive.Name == "Personal") {
			t.Errorf("%s at %s legacy=%v, want %s", d.resolved.Drive.Name, d.target, d.legacy, want[d.resolved.Drive.Name])
		}
	}
}

// TestAuthorizeRequestDrives_ForeignAndRandomAreOneAnswer: another person's
// drive and an id that names nothing get the same refusal, never a mount and
// never a different drive in its place.
func TestAuthorizeRequestDrives_ForeignAndRandomAreOneAnswer(t *testing.T) {
	ds := newDriveSet()
	srv, _ := multiDriveServer(ds.personal, ds.team, ds.foreign)
	for _, id := range []string{ds.foreign.Drive.ID.String(), uuid.NewString()} {
		drives, ref := authorizeDrives(srv, driveMemberCtx([]string{"eng"}, false), drivesRequest(id))
		if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(authz.ReasonDriveNotFound) || ref.body.Error != driveNotFoundMsg(id) || drives != nil {
			t.Fatalf("drives[%s] = %+v, %+v; want 422 %s %q", id, drives, ref, string(authz.ReasonDriveNotFound), driveNotFoundMsg(id))
		}
	}
}

// TestAuthorizeRequestDrives_PausedAndConflicts: a paused winner refuses; two
// drives deriving one path refuse rather than one hiding the other.
func TestAuthorizeRequestDrives_PausedAndConflicts(t *testing.T) {
	ds := newDriveSet()
	ds.team.Grant.Enabled = false
	twin := winnerOf("shared docs", types.DriveBackendHostPath, types.CapabilitySubjectAll, "", true)
	srv, _ := multiDriveServer(ds.personal, ds.team, ds.share, twin)
	ctx := driveMemberCtx([]string{"eng"}, false)

	_, ref := authorizeDrives(srv, ctx, drivesRequest(ds.personal.Drive.ID.String(), ds.team.Drive.ID.String()))
	if ref == nil || ref.drive == nil || ref.drive.reason != driveRefusalPaused {
		t.Errorf("paused team drive = %+v, want the paused refusal", ref)
	}
	_, ref = authorizeDrives(srv, ctx, drivesRequest(ds.share.Drive.ID.String(), twin.Drive.ID.String()))
	want := driveTargetConflictMsg("Shared Docs", "shared docs", runner.SharedRoot+"/shared-docs")
	if ref == nil || ref.body.Reason != string(authz.ReasonDriveTargetConflict) || ref.body.Error != want {
		t.Errorf("two shares on one slug = %+v, want 422 %s %q", ref, string(authz.ReasonDriveTargetConflict), want)
	}
}

// TestAuthorizeRequestDrives_StaleGroups: with the group snapshot unanswerable,
// a non-user-tier winner could be outranked by a hidden group grant, so the
// request refuses; drives won by the person's own grants still resolve.
func TestAuthorizeRequestDrives_StaleGroups(t *testing.T) {
	ds := newDriveSet()
	srv, st := multiDriveServer(ds.personal, ds.share)
	st.hasGroupTier = true
	stale := driveMemberCtx([]string{"eng"}, true)
	_, ref := authorizeDrives(srv, stale, drivesRequest(ds.personal.Drive.ID.String()))
	if ref == nil || ref.body.Reason != reasonGroupsSnapshotStale {
		t.Errorf("stale snapshot beside an all-tier winner = %+v, want %s", ref, reasonGroupsSnapshotStale)
	}
	srv, st = multiDriveServer(ds.personal)
	st.hasGroupTier = true
	if drives, ref := authorizeDrives(srv, stale, drivesRequest(ds.personal.Drive.ID.String())); ref != nil || len(drives) != 1 {
		t.Errorf("stale snapshot, user-tier winners only = %+v, %+v; want the drive", drives, ref)
	}
}

// TestSeedRequestDrives_PerAttachmentModeAndOneFailureRefusesAll: read_only
// narrows only its own drive; widening a read-only grant refuses; one drive
// that cannot mount here refuses the whole request and mounts nothing.
func TestSeedRequestDrives_PerAttachmentModeAndOneFailureRefusesAll(t *testing.T) {
	ds := newDriveSet()
	other := winnerOf("Other", types.DriveBackendDockerVolume, types.CapabilitySubjectUser, "sub-drive-bob", true)
	k8s := winnerOf("Cluster", types.DriveBackendK8sPVC, types.CapabilitySubjectUser, "sub-drive-bob", true)
	srv, _ := multiDriveServer(ds.personal, ds.team, other, k8s)
	ctx := driveMemberCtx([]string{"eng"}, false)
	seed := func(req createRunRequest) ([]types.DriveMount, bool, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		_, mounts, ok := srv.seedRequestDrives(w, httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx), req, governanceCeiling{})
		return mounts, ok, w
	}

	yes, no := true, false
	req := drivesRequest(ds.personal.Drive.ID.String(), other.Drive.ID.String())
	req.Drives[0].ReadOnly = &yes
	mounts, ok, w := seed(req)
	if !ok || len(mounts) != 2 || !mounts[0].ReadOnly || mounts[1].ReadOnly {
		t.Fatalf("narrowed first drive = %+v ok=%v (%s); want ro, rw", mounts, ok, w.Body)
	}
	if mounts[0].Target != runner.DriveTarget || mounts[1].Target != runner.DrivesRoot+"/other" {
		t.Errorf("targets = %s, %s", mounts[0].Target, mounts[1].Target)
	}

	req = drivesRequest(ds.team.Drive.ID.String())
	req.Drives[0].ReadOnly = &no
	if mounts, ok, w = seed(req); ok || mounts != nil || !strings.Contains(w.Body.String(), driveRefusalReadOnly) {
		t.Errorf("read_only:false on a read-only grant = %v %v %s, want the read_only refusal", mounts, ok, w.Body)
	}

	if mounts, ok, w = seed(drivesRequest(ds.personal.Drive.ID.String(), k8s.Drive.ID.String())); ok || mounts != nil ||
		w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), driveRefusalBackendElsewhere) {
		t.Errorf("one unmountable drive = %v %v %d %s, want the whole request refused", mounts, ok, w.Code, w.Body)
	}
}

// TestStepDriveEligibility_PersonsOwnRunner: no drive mounts on a person's own
// runner, so a run placement left on one is refused, named; a run placed on a
// remote-hosted runner is not.
func TestStepDriveEligibility_PersonsOwnRunner(t *testing.T) {
	ds := newDriveSet()
	drive := runDrive{resolved: types.ResolvedDrive{Drive: ds.team.Drive}, target: runner.DrivesRoot + "/team-data"}
	for kind, want := range map[types.Placement]bool{types.PlacementLocal: false, types.PlacementRemote: true} {
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), drives: []runDrive{drive},
			place: runPlacement{pooled: true, placement: kind, row: types.RunnerPool{Scope: types.RunnerPoolUser}}}
		if got := (&Server{}).stepDriveEligibility(f, &foldRecorder{}); got != want {
			t.Fatalf("%s: step = %v, want %v (%s)", kind, got, want, w.Body)
		}
		if !want && (w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), string(authz.ReasonDriveNotInPool)) ||
			!strings.Contains(w.Body.String(), "Team Data: not available in this pool's runners.")) {
			t.Errorf("%s refusal = %d %s", kind, w.Code, w.Body)
		}
	}
}

// TestStartFolderOnAListedDrive: start_folder.drive_id names one of drives[]
// and resolves to that drive's path; any other id is refused by the request
// alone.
func TestStartFolderOnAListedDrive(t *testing.T) {
	ds := newDriveSet()
	id := ds.team.Drive.ID.String()
	req := drivesRequest(id)
	req.StartFolder = &client.StartFolder{Kind: client.StartFolderAttachment, DriveID: id, Subpath: "src"}
	if ref := startFolderRefusal(req); ref != nil {
		t.Fatalf("drive_id of drives[] refused: %+v", ref)
	}
	req.StartFolder.DriveID = uuid.NewString()
	if ref := startFolderRefusal(req); ref == nil || ref.body.Reason != reasonStartFolderInvalid {
		t.Errorf("drive_id not in drives[] = %+v, want %s", ref, reasonStartFolderInvalid)
	}

	req.StartFolder.DriveID = id
	targets := driveTargets([]runDrive{{resolved: types.ResolvedDrive{Drive: ds.team.Drive, Writable: true}, target: runner.DrivesRoot + "/team-data", id: id}})
	facts, dir, ref := (&Server{}).runAttachments(req, types.RunPolicySpec{}, nil, workspaceSourceIndex{}, workspaceSeed{}, targets)
	if ref != nil || dir != runner.DrivesRoot+"/team-data/src" {
		t.Fatalf("start folder = %q, %+v", dir, ref)
	}
	if len(facts) != 1 || facts[0].DriveID != id || !facts[0].StartsHere || facts[0].Name != "Team Data" || facts[0].Kind != attachmentDrive {
		t.Errorf("mount map = %+v", facts)
	}
}

// TestOverlap_PluralDriveTargets: a listed drive's path joins the overlap tree,
// nested either way against a workspace.
func TestOverlap_PluralDriveTargets(t *testing.T) {
	ds := newDriveSet()
	// Two drives: the overlap with the SECOND one is the one a [0]-only tree misses.
	drive := driveTargets([]runDrive{
		{resolved: types.ResolvedDrive{Drive: ds.personal.Drive}, target: runner.SharedRoot + "/personal", id: ds.personal.Drive.ID.String()},
		{resolved: types.ResolvedDrive{Drive: ds.team.Drive}, target: runner.DrivesRoot + "/team-data", id: ds.team.Drive.ID.String()},
	})
	home := folderWS("home", "/srv/home")
	for _, target := range []string{runner.DrivesRoot, runner.DrivesRoot + "/team-data/inner"} {
		spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/home", Target: target}}}
		_, _, ref := (&Server{}).runAttachments(createRunRequest{}, spec, []types.Workspace{home},
			indexWorkspacesBySource([]types.Workspace{home}), workspaceSeed{attached: []types.Workspace{home}}, drive)
		if ref == nil || ref.body.Reason != reasonWorkspaceTargetOverlap || !strings.Contains(ref.body.Error, runner.DrivesRoot+"/team-data") {
			t.Errorf("workspace at %s beside the drive = %+v, want %s naming the drive's path", target, ref, reasonWorkspaceTargetOverlap)
		}
	}
}

// TestMeDrivesCatalogue: the member's own rows with their source, section,
// derived path, legacy mark and reason; nobody else's drive; no host root.
func TestMeDrivesCatalogue(t *testing.T) {
	ds := newDriveSet()
	ds.team.Grant.Enabled = false
	k8s := winnerOf("Cluster", types.DriveBackendK8sPVC, types.CapabilitySubjectUser, "sub-drive-bob", true)
	srv, _ := multiDriveServer(ds.personal, ds.team, ds.share, k8s, ds.foreign)
	w := httptest.NewRecorder()
	srv.handleListMeDrives(w, httptest.NewRequest(http.MethodGet, "/api/v1/me/drives", nil).WithContext(driveMemberCtx([]string{"eng"}, false)))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /me/drives = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "/srv/share") || strings.Contains(w.Body.String(), "Theirs") {
		t.Errorf("the catalogue discloses a host root or another person's drive: %s", w.Body)
	}
	var got client.MeDriveCatalogue
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []client.MeDrive{
		{Name: "Personal", Section: "user", Source: "personal", Target: runner.DriveTarget, Legacy: true},
		{Name: "Team Data", Section: "user", Source: "team", Target: runner.DrivesRoot + "/team-data", UnavailableReason: "paused"},
		// No share root is configured on this server, so the share cannot bind here.
		{Name: "Shared Docs", Section: "shared", Source: "organisation", Target: runner.SharedRoot + "/shared-docs", UnavailableReason: "unmountable"},
		{Name: "Cluster", Section: "user", Source: "personal", Target: runner.DrivesRoot + "/cluster", UnavailableReason: "unmountable"},
	}
	if len(got.Drives) != len(want) || got.PoolChecked {
		t.Fatalf("catalogue = %+v, want %d rows and no pool check", got, len(want))
	}
	for i, w := range want {
		g := got.Drives[i]
		if g.Name != w.Name || g.Section != w.Section || g.Source != w.Source || g.Target != w.Target || g.Legacy != w.Legacy || g.UnavailableReason != w.UnavailableReason {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
	}
}

// TestMeDrivesCatalogue_DoorAndStale: a shut profile door lists nothing and
// names the profile; an unanswerable group snapshot lists nothing and says so.
func TestMeDrivesCatalogue_DoorAndStale(t *testing.T) {
	ds := newDriveSet()
	srv, st := multiDriveServer(ds.personal, ds.share)
	st.profile = &types.GovernanceUnit{Name: "contractors", Limits: types.GovernanceLimits{DenyUserDrive: true}}
	get := func(ctx context.Context) client.MeDriveCatalogue {
		w := httptest.NewRecorder()
		srv.handleListMeDrives(w, httptest.NewRequest(http.MethodGet, "/api/v1/me/drives", nil).WithContext(ctx))
		var out client.MeDriveCatalogue
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Drives == nil {
			t.Fatalf("body %s: %v (drives must be [], never null)", w.Body, err)
		}
		return out
	}
	if got := get(driveMemberCtx(nil, false)); got.DeniedByUnit != "contractors" || len(got.Drives) != 0 {
		t.Errorf("denied = %+v", got)
	}
	st.profile = nil
	st.hasGroupTier = true
	if got := get(driveMemberCtx([]string{"eng"}, true)); got.Unavailable != driveUnavailableGroups || len(got.Drives) != 0 {
		t.Errorf("stale = %+v", got)
	}
}

// TestDriveUnavailableHere_Pool: a checked self-hosted pool makes a mountable
// drive not_in_pool; a remote-provided one leaves it attachable.
func TestDriveUnavailableHere_Pool(t *testing.T) {
	ds := newDriveSet()
	srv, _ := multiDriveServer()
	d := reachableDrive{winner: ds.team, resolved: &types.ResolvedDrive{Drive: ds.team.Drive}}
	self := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Scope: types.RunnerPoolUser}}
	remote := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Scope: types.RunnerPoolOrganization}}
	ctx := context.Background()
	if got := srv.driveUnavailableHere(ctx, d, self, true, false); got != client.MeDriveUnavailableNotInPool {
		t.Errorf("self-hosted = %q", got)
	}
	if got := srv.driveUnavailableHere(ctx, d, remote, true, false); got != "" {
		t.Errorf("remote-provided = %q", got)
	}
	if got := srv.driveUnavailableHere(ctx, d, self, false, false); got != "" {
		t.Errorf("unchecked pool = %q", got)
	}
	// A profile that denies remote-hosted runners leaves a drive nowhere to mount, in any pool.
	// It says the profile, not the pool, and needs no pool to say it.
	if got := srv.driveUnavailableHere(ctx, d, remote, true, true); got != client.MeDriveUnavailablePlacementDenied {
		t.Errorf("remote-provided, remote denied = %q", got)
	}
	if got := srv.driveUnavailableHere(ctx, d, self, false, true); got != client.MeDriveUnavailablePlacementDenied {
		t.Errorf("unchecked pool, remote denied = %q", got)
	}
}

// TestDriveCreateAudit: run.create records each drives[] drive by id, name,
// path and mode, never a host root or object name; a run with no drive, or
// with the legacy drive.enabled drive, records no drives key.
func TestDriveCreateAudit(t *testing.T) {
	ds := newDriveSet()
	data := map[string]any{}
	stampDriveAudit(data, []runDrive{{resolved: types.ResolvedDrive{Drive: ds.share.Drive, ObjectName: "/srv/share/bob", Writable: true},
		target: runner.SharedRoot + "/shared-docs", id: ds.share.Drive.ID.String()}})
	got := string(mustJSON(data))
	if strings.Contains(got, "/srv/share") {
		t.Errorf("run.create drives = %s leaks the host path", got)
	}
	for _, key := range []string{`"drive_id"`, `"name":"Shared Docs"`, `"target":"/home/agent/shared/shared-docs"`, `"read_only":false`} {
		if !strings.Contains(got, key) {
			t.Errorf("run.create drives = %s, missing %s", got, key)
		}
	}
	for _, drives := range [][]runDrive{nil, {{target: runner.DriveTarget, legacy: true}}} {
		data := map[string]any{}
		if stampDriveAudit(data, drives); len(data) != 0 {
			t.Errorf("drives %+v recorded %v", drives, data)
		}
	}
}

// TestStepDrive_K8sShareBesideManagedAtEveryDoor: a shared Kubernetes drive
// beside a managed one is refused, named, at the preview, the preflight and
// create, before anything is created (the substrate cannot mount the pair).
func TestStepDrive_K8sShareBesideManagedAtEveryDoor(t *testing.T) {
	managed := winnerOf("Cluster Home", types.DriveBackendK8sPVC, types.CapabilitySubjectUser, "sub-drive-bob", true)
	share := winnerOf("Cluster Share", types.DriveBackendK8sPVCStatic, types.CapabilitySubjectAll, "", true)
	srv, _ := multiDriveServer(managed, share)
	srv.cfg.RunnerTarget = "k8s"
	req := drivesRequest(managed.Drive.ID.String(), share.Drive.ID.String())
	for _, mode := range []foldMode{foldPreview, foldPreflight, foldCreate} {
		w := httptest.NewRecorder()
		f := &runFold{mode: mode, w: w, req: &req,
			r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(driveMemberCtx([]string{"eng"}, false))}
		if srv.stepDrive(f, &foldRecorder{}) {
			t.Fatalf("mode %d: a share beside a managed drive passed", mode)
		}
		want := driveShareBesideManagedMsg("Cluster Share", "Cluster Home")
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), reasonDriveShareBesideManaged) || !strings.Contains(w.Body.String(), want) {
			t.Errorf("mode %d: %d %s, want 422 %s %q", mode, w.Code, w.Body, reasonDriveShareBesideManaged, want)
		}
	}
	if drives, ref := authorizeDrives(srv, driveMemberCtx(nil, false), drivesRequest(managed.Drive.ID.String())); ref != nil || len(drives) != 1 {
		t.Errorf("the managed drive alone = %+v, %+v; want it attached", drives, ref)
	}
}

// TestDriveDenialsAreAudited: drive_not_found, drive_target_conflict and
// drive_not_in_pool each leave an authz.denied row; another person's drive and
// an id that names nothing leave the same row.
func TestDriveDenialsAreAudited(t *testing.T) {
	ds := newDriveSet()
	twin := winnerOf("shared docs", types.DriveBackendHostPath, types.CapabilitySubjectAll, "", true)
	srv, _ := multiDriveServer(ds.personal, ds.share, twin, ds.foreign)
	audit := srv.cfg.Audit.(*recRecorder)
	ctx := driveMemberCtx([]string{"eng"}, false)
	deny := func(req createRunRequest) types.AuditEvent {
		t.Helper()
		audit.events = nil
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
		_, ref := srv.authorizeRequestDrives(r, req, governanceCeiling{})
		if !ref.write(srv, httptest.NewRecorder(), r) || len(audit.events) != 1 || audit.events[0].Action != "authz.denied" {
			t.Fatalf("refusal %+v wrote events %+v, want one authz.denied", ref, audit.events)
		}
		return audit.events[0]
	}
	foreign := deny(drivesRequest(ds.foreign.Drive.ID.String()))
	random := deny(drivesRequest(uuid.NewString()))
	if foreign.Target != "runs.drive" || foreign.Target != random.Target || string(foreign.Data) != string(random.Data) {
		t.Errorf("foreign row %s %s != random row %s %s", foreign.Target, foreign.Data, random.Target, random.Data)
	}
	if strings.Contains(string(foreign.Data), ds.foreign.Drive.ID.String()) || strings.Contains(string(foreign.Data), "Theirs") {
		t.Errorf("the drive_not_found row names the drive: %s", foreign.Data)
	}
	if ev := deny(drivesRequest(ds.share.Drive.ID.String(), twin.Drive.ID.String())); !strings.Contains(string(ev.Data), string(authz.ReasonDriveTargetConflict)) {
		t.Errorf("target conflict row = %s", ev.Data)
	}

	audit.events = nil
	w := httptest.NewRecorder()
	f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx),
		drives: []runDrive{{resolved: types.ResolvedDrive{Drive: ds.share.Drive}}},
		place:  runPlacement{pooled: true, placement: types.PlacementLocal, row: types.RunnerPool{Scope: types.RunnerPoolUser}}}
	if srv.stepDriveEligibility(f, &foldRecorder{}) || len(audit.events) != 1 || !strings.Contains(string(audit.events[0].Data), string(authz.ReasonDriveNotInPool)) {
		t.Errorf("not_in_pool events = %+v", audit.events)
	}
}

// TestMeDrivesCatalogue_DrivesOff: with storage.user_drive.disabled the
// catalogue lists nothing a launch would refuse.
func TestMeDrivesCatalogue_DrivesOff(t *testing.T) {
	ds := newDriveSet()
	srv, st := multiDriveServer(ds.personal, ds.share)
	st.site = types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Storage: &types.StorageProviders{UserDrive: &types.UserDriveProvider{Disabled: true}}}}
	w := httptest.NewRecorder()
	srv.handleListMeDrives(w, httptest.NewRequest(http.MethodGet, "/api/v1/me/drives", nil).WithContext(driveMemberCtx([]string{"eng"}, false)))
	var got client.MeDriveCatalogue
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Drives) != 0 || got.Unavailable != driveUnavailableUnknown {
		t.Errorf("drives off = %+v, want no rows and %q", got, driveUnavailableUnknown)
	}
}

// grantVerbStore records which grant verb a write took; a replace reports
// removed as the drives the subject lost.
type grantVerbStore struct {
	*driveStore
	verb    string
	removed []uuid.UUID
}

func (s *grantVerbStore) ReplaceUserDriveGrant(_ context.Context, g types.UserDriveGrant, _ bool) (types.UserDriveGrant, []uuid.UUID, error) {
	s.verb = "replace"
	return g, s.removed, nil
}

func (s *grantVerbStore) AddUserDriveGrant(_ context.Context, g types.UserDriveGrant, _ bool) (types.UserDriveGrant, error) {
	s.verb = "add"
	return g, nil
}

// TestWriteUserDriveGrant_Verb: an allocation replaces the subject's drive
// unless the request says additional.
func TestWriteUserDriveGrant_Verb(t *testing.T) {
	for additional, want := range map[bool]string{false: "replace", true: "add"} {
		st := &grantVerbStore{driveStore: &driveStore{}}
		srv := New(Config{Store: st, RunnerTarget: "docker"})
		if _, _, err := srv.writeUserDriveGrant(context.Background(), types.UserDriveGrant{}, true, additional); err != nil || st.verb != want {
			t.Errorf("additional=%v took %q (%v), want %q", additional, st.verb, err, want)
		}
	}
}

// TestUpsertUserDriveGrant_AuditsRemovedDrives: a replacing write's
// drive.grant.write row names the drives the subject lost.
func TestUpsertUserDriveGrant_AuditsRemovedDrives(t *testing.T) {
	lost := uuid.New()
	st := &grantVerbStore{driveStore: &driveStore{}, removed: []uuid.UUID{lost}}
	audit := &recRecorder{}
	srv := New(Config{Store: st, Audit: audit, RunnerTarget: "docker"})
	body := `{"subject_type":"user","subject":"alice@example.com","drive_id":"` + uuid.NewString() + `","priority":0}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/drives/grants", strings.NewReader(body)).
		WithContext(operatorCtx("sub-admin", "admin@example.com", "admin"))
	srv.handleUpsertUserDriveGrant(w, r)
	if w.Code/100 != 2 || len(audit.events) != 1 || !strings.Contains(string(audit.events[0].Data), `"removed_drive_ids":["`+lost.String()+`"]`) {
		t.Fatalf("%d %s, events %+v; want a drive.grant.write row naming %s", w.Code, w.Body, audit.events, lost)
	}
}
