// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// meDrivesOf reads GET /me/drives as ctx's caller.
func meDrivesOf(t *testing.T, srv *Server, ctx context.Context) client.MeDriveCatalogue {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handleListMeDrives(w, httptest.NewRequest(http.MethodGet, "/api/v1/me/drives", nil).WithContext(ctx))
	var out client.MeDriveCatalogue
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("GET /me/drives = %d %s", w.Code, w.Body)
	}
	return out
}

// catalogueRunner is a member-drive substrate that also answers the
// catalogue's capability read.
func catalogueRunner() *memberDriveRunner {
	return &memberDriveRunner{reclaimRunner: reclaimRunner{Runner: &fakeRunner{}}}
}

// TestMemberDriveOwnerSubOnly: a member drive reaches its owner by the exact
// sign-in subject. Bob, whose email equals Alice's sub, does not see it, cannot
// attach it and cannot delete it; Alice sees it owned, never legacy, and
// attaches it at its own path.
func TestMemberDriveOwnerSubOnly(t *testing.T) {
	st := newMemberDriveStore()
	srv, _ := memberDriveServer(st, catalogueRunner(), "docker")
	alice := personCtx(aliceSub)
	bob := withOIDCGroups(operatorCtx(bobSub, aliceSub, oidc.RoleUser), []string{"eng"})
	w := createMemberDrive(srv, alice, "Investigation files")
	var made client.MeDrive
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &made) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}

	got := meDrivesOf(t, srv, alice)
	if len(got.Drives) != 1 {
		t.Fatalf("Alice's catalogue = %+v, want her one drive", got.Drives)
	}
	if d := got.Drives[0]; d.ID != made.ID || !d.Owned || d.Legacy || d.Section != client.MeDriveSectionUser ||
		d.Source != client.MeDriveSourcePersonal || d.Target != runner.DrivesRoot+"/investigation-files" || !d.Writable {
		t.Errorf("Alice's row = %+v", d)
	}
	if c := got.Create; c == nil || !c.Allowed || c.Count != 1 || c.Limit != 3 {
		t.Errorf("Alice's create block = %+v, want allowed 1 of 3", c)
	}
	if b := meDrivesOf(t, srv, bob); len(b.Drives) != 0 || b.Create == nil || b.Create.Count != 0 {
		t.Errorf("Bob's catalogue = %+v (create %+v): Alice's drive reached him", b.Drives, b.Create)
	}

	if _, f := authorizeDrives(srv, bob, drivesRequest(made.ID)); f == nil || f.body.Reason != string(authz.ReasonDriveNotFound) {
		t.Errorf("Bob attaching Alice's drive = %+v, want drive_not_found", f)
	}
	drives, f := authorizeDrives(srv, alice, drivesRequest(made.ID))
	if f != nil || len(drives) != 1 || drives[0].legacy || drives[0].target != made.Target || drives[0].resolved.Drive.OwnerSubject != aliceSub {
		t.Fatalf("Alice attaching her drive = %+v %+v", drives, f)
	}
	if owners := memberDriveOwners(drives); len(owners) != 1 || owners[drives[0].resolved.Drive.ID] != aliceSub {
		t.Errorf("dispatch's member drives = %v", owners)
	}
	wantReason(t, memberCall(bob, srv.handleDeleteMyDrive, http.MethodDelete, "/api/v1/me/drives/"+made.ID, "", map[string]string{"id": made.ID}),
		http.StatusNotFound, string(authz.ReasonDriveNotFound))

	// A drive being deleted is gone from the catalogue and from drives[].
	id := uuid.MustParse(made.ID)
	d := st.drives[id]
	at := time.Now()
	d.DeleteStartedAt = &at
	st.drives[id] = d
	if a := meDrivesOf(t, srv, alice); len(a.Drives) != 0 || a.Create.Count != 1 {
		t.Errorf("a drive being deleted = %+v (create %+v)", a.Drives, a.Create)
	}
	if _, f := authorizeDrives(srv, alice, drivesRequest(made.ID)); f == nil || f.body.Reason != string(authz.ReasonDriveNotFound) {
		t.Errorf("attaching a drive being deleted = %+v, want drive_not_found", f)
	}
}

// TestMemberDriveCatalogueCreateBlock: the create block answers create's own
// doors, writes no refusal row, and is absent for a caller who is not a person.
func TestMemberDriveCatalogueCreateBlock(t *testing.T) {
	alice := personCtx(aliceSub)
	cases := []struct {
		name  string
		setup func(st *memberDriveStore, rn *memberDriveRunner)
		want  client.MemberDriveCreate
	}{
		{"switch off", func(st *memberDriveStore, _ *memberDriveRunner) { st.site = types.SiteConfig{} },
			client.MemberDriveCreate{Reason: client.MemberDriveCreateDisabled}},
		{"profile door", func(st *memberDriveStore, _ *memberDriveRunner) {
			st.profile = &types.GovernanceUnit{Name: "contractors", Limits: types.GovernanceLimits{DenyMemberDrives: true}}
		}, client.MemberDriveCreate{Reason: client.MemberDriveCreateDeniedByUnit, DeniedByUnit: "contractors", Limit: 3}},
		{"undeletable", func(_ *memberDriveStore, rn *memberDriveRunner) {
			rn.lifecycleErr = runner.ErrDriveLifecycleUnavailable
		},
			client.MemberDriveCreate{Reason: client.MemberDriveCreateDeleteUnavailable, Limit: 3}},
		{"limit reached", func(st *memberDriveStore, _ *memberDriveRunner) { st.site = memberDriveSite(1, 0) },
			client.MemberDriveCreate{Reason: client.MemberDriveCreateLimitReached, Count: 1, Limit: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemberDriveStore()
			rn := catalogueRunner()
			srv, rec := memberDriveServer(st, rn, "docker")
			if tc.want.Count > 0 {
				if w := createMemberDrive(srv, alice, "Notes"); w.Code != http.StatusCreated {
					t.Fatalf("create = %d %s", w.Code, w.Body)
				}
			}
			tc.setup(st, rn)
			before := countAction(rec, "authz.denied")
			if got := meDrivesOf(t, srv, alice).Create; got == nil || *got != tc.want {
				t.Errorf("create = %+v, want %+v", got, tc.want)
			}
			if countAction(rec, "authz.denied") != before {
				t.Error("the catalogue's display read wrote an authz.denied row")
			}
		})
	}
	srv, _ := memberDriveServer(newMemberDriveStore(), catalogueRunner(), "docker")
	for name, ctx := range map[string]context.Context{
		"admin token": context.Background(),
		"device":      context.WithValue(alice, deviceCtxKey{}, types.Device{ID: uuid.New()}),
		"scim":        withSCIMCaller(alice, "primary"),
	} {
		if got := meDrivesOf(t, srv, ctx).Create; got != nil {
			t.Errorf("%s: create = %+v, want absent", name, got)
		}
	}
}

// TestMemberDriveNeverOnSelfHosted: a person's own drive is not_in_pool on a
// self-hosted pool and refuses a run there, by its own rule, and a local
// runner refuses it even with every local path verified.
func TestMemberDriveNeverOnSelfHosted(t *testing.T) {
	member := types.UserDrive{ID: uuid.New(), Name: "Mine", Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, ObjectScheme: types.DriveObjectSchemeID, Writable: true, OwnerSubject: aliceSub}
	admin := member
	admin.OwnerSubject = ""
	self := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Scope: types.RunnerPoolUser}}
	remote := store.RunnerPoolCatalogueRow{Pool: types.RunnerPool{Scope: types.RunnerPoolUser}, ExecutorIDs: []string{uuid.NewString()}}
	if !driveNotInPool(self, member) || driveNotInPool(remote, member) {
		t.Error("driveNotInPool must hold a person's own drive off a self-hosted pool, and only there")
	}

	srv, _ := multiDriveServer()
	d := reachableDrive{owned: true, winner: store.UserDriveWinner{Drive: member}, resolved: &types.ResolvedDrive{Drive: member, Writable: true}}
	if got := srv.driveUnavailableHere(context.Background(), d, self, true, false); got != client.MeDriveUnavailableNotInPool {
		t.Errorf("catalogue on a self-hosted pool = %q, want not_in_pool", got)
	}

	w := httptest.NewRecorder()
	f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil),
		drives: []runDrive{{resolved: types.ResolvedDrive{Drive: member}, target: d.target()}},
		place:  runPlacement{pooled: true, placement: types.PlacementLocal}}
	if (&Server{}).stepDriveEligibility(f, &foldRecorder{}) || w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), string(authz.ReasonDriveNotInPool)) || !strings.Contains(w.Body.String(), driveNotInPoolMsg("Mine")) {
		t.Errorf("attach on a self-hosted pool = %d %s, want 422 drive_not_in_pool", w.Code, w.Body)
	}

	mount, err := memberDriveMount(member)
	if err != nil {
		t.Fatal(err)
	}
	plan := placement.LocalPlan{Spec: runner.SandboxSpec{Drives: []types.DriveMount{mount}},
		VerifiedLocalPaths: map[string]bool{"SandboxSpec.Drives[0]": true}}
	if _, ref := placement.LocalEligibility(plan); ref == nil || ref.Field != "SandboxSpec.Drives[0]" {
		t.Errorf("a local runner with the path verified = %+v, want the drive refused", ref)
	}
	if !memberDriveNeverOnPool(self, member) || memberDriveNeverOnPool(remote, member) || memberDriveNeverOnPool(self, admin) {
		t.Error("the permanent rule must hold a person's own drive, and only it, off a self-hosted pool")
	}
}

// memberRecheckStore answers dispatch's re-read of a member drive.
type memberRecheckStore struct {
	*dispatchTestStore
	drives map[uuid.UUID]types.UserDrive
}

// SetRunImage is the image write a launch makes.
func (*memberRecheckStore) SetRunImage(context.Context, uuid.UUID, string) error { return nil }

func (s *memberRecheckStore) GetMemberDriveOf(_ context.Context, owner string, id uuid.UUID) (types.UserDrive, error) {
	if d, ok := s.drives[id]; ok && d.OwnerSubject == owner {
		return d, nil
	}
	return types.UserDrive{}, store.ErrNotFound
}

// TestMemberDriveDispatchRecheck: dispatch re-reads each member drive under the
// run's owner right before CreateSandbox. Deleted, being deleted, not the run
// owner's, or a store that cannot re-read fails the run drive_not_found with no
// CreateSandbox call; present mounts it.
func TestMemberDriveDispatchRecheck(t *testing.T) {
	id := uuid.New()
	row := types.UserDrive{ID: id, Name: "Mine", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub}
	at := time.Now()
	tomb := row
	tomb.DeleteStartedAt = &at
	cases := []struct {
		name     string
		drives   map[uuid.UUID]types.UserDrive // nil: the store cannot re-read
		owner    string
		runOwner string
		mounts   bool
	}{
		{"present", map[uuid.UUID]types.UserDrive{id: row}, aliceSub, aliceSub, true},
		{"deleted", map[uuid.UUID]types.UserDrive{}, aliceSub, aliceSub, false},
		{"being deleted", map[uuid.UUID]types.UserDrive{id: tomb}, aliceSub, aliceSub, false},
		{"another owner", map[uuid.UUID]types.UserDrive{id: row}, bobSub, aliceSub, false},
		{"another person's run", map[uuid.UUID]types.UserDrive{id: row}, aliceSub, bobSub, false},
		{"store cannot re-read", nil, aliceSub, aliceSub, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{}
			srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
			if tc.drives != nil {
				srv.cfg.Store = &memberRecheckStore{dispatchTestStore: st, drives: tc.drives}
			}
			run.Task, run.CreatedBy, st.run.CreatedBy = "", tc.runOwner, tc.runOwner
			mount := types.DriveMount{DriveID: id, Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-drive-x", DriveName: "Mine",
				HomeName: "d-x", Target: runner.DrivesRoot + "/mine", Enforcement: types.StorageEnforcementNone}
			srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
				RunToken: "run-token", Image: "wardyn/claude-code:latest", Drives: []types.DriveMount{mount},
				MemberDrives: map[uuid.UUID]string{id: tc.owner},
			})
			if tc.mounts {
				if fr.createCalls != 1 || len(fr.lastSpec.Drives) != 1 {
					t.Fatalf("CreateSandbox calls = %d, drives %+v; want the drive mounted", fr.createCalls, fr.lastSpec.Drives)
				}
				return
			}
			if fr.createCalls != 0 {
				t.Fatalf("CreateSandbox ran %d time(s) for a gone member drive", fr.createCalls)
			}
			if st.State() != types.RunFailed || st.FailureHint() != driveNotFoundMsg(id.String()) {
				t.Errorf("run = %s %q, want FAILED with the drive_not_found sentence", st.State(), st.FailureHint())
			}
			ev := findAudit(audit.events, run.ID, "run.create", "failure")
			if ev == nil || !strings.Contains(string(ev.Data), `"reason":"drive_not_found"`) {
				t.Errorf("run.create failure row = %+v", ev)
			}
			if findAudit(audit.events, run.ID, "run.drive.mount", "success") != nil {
				t.Error("a refused member drive was audited as mounted")
			}
		})
	}
}

// TestMemberDriveLaunchCarriesTheOwnersToDispatch: the launch both create and the pool queue's start build
// from the fold hands dispatch each member drive's owner, so a drive deleted before the sandbox is created
// fails the run. A launch that dropped the owners would skip the re-read and create the sandbox.
func TestMemberDriveLaunchCarriesTheOwnersToDispatch(t *testing.T) {
	id := uuid.New()
	row := types.UserDrive{ID: id, Name: "Mine", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub}
	fr := &fakeRunner{}
	srv, st, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.Store = &memberRecheckStore{dispatchTestStore: st, drives: map[uuid.UUID]types.UserDrive{}} // the drive is gone
	run.Task, run.CreatedBy, st.run.CreatedBy = "", aliceSub, aliceSub
	mount := types.DriveMount{DriveID: id, Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-drive-x", DriveName: "Mine",
		HomeName: "d-x", Target: runner.DrivesRoot + "/mine", Enforcement: types.StorageEnforcementNone}
	f := runFold{drives: []runDrive{{resolved: types.ResolvedDrive{Drive: row}}}, driveMounts: []types.DriveMount{mount},
		adoGrade: adoEntraUngraded(), bedrockGrade: bedrockCredUngraded()}
	l := f.launch(createRunRequest{Agent: "claude-code"}, types.RunPolicySpec{MinConfinementClass: types.CC1}, governanceCeiling{}, grantWiring{}, "run-token", run)
	if got := l.memberDrives[id]; got != aliceSub {
		t.Fatalf("launch member drives = %v, want %s's drive %s", l.memberDrives, aliceSub, id)
	}
	srv.finishCreateRunLaunch(context.Background(), l)
	if fr.createCalls != 0 {
		t.Fatalf("CreateSandbox ran %d time(s) for a member drive that was gone", fr.createCalls)
	}
	if st.State() != types.RunFailed || st.FailureHint() != driveNotFoundMsg(id.String()) {
		t.Errorf("run = %s %q, want FAILED with the drive_not_found sentence", st.State(), st.FailureHint())
	}
}

// TestTemplateRefusesMemberDriveInSharedScope: an organisation or group
// template naming a person's own drive is refused at that path, and the answer
// never carries the drive's name; an admin drive and a person's own template
// pass.
func TestTemplateRefusesMemberDriveInSharedScope(t *testing.T) {
	st := newMemberDriveStore()
	srv, _ := memberDriveServer(st, catalogueRunner(), "docker")
	mine := types.UserDrive{ID: uuid.New(), Name: "Alice private notes", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub}
	team := types.UserDrive{ID: uuid.New(), Name: "Team data", Backend: types.DriveBackendDockerVolume}
	st.drives[mine.ID], st.drives[team.ID] = mine, team
	doc := []byte(`{"intent":{"drives":[{"drive_id":"` + team.ID.String() + `"},{"drive_id":"` + mine.ID.String() + `"}],` +
		`"start_folder":{"kind":"attachment","drive_id":"` + mine.ID.String() + `"}}}`)
	for scope, refused := range map[types.TemplateOwner]bool{
		{Scope: types.TemplateScopeGroup, Group: "eng"}:      true,
		{Scope: types.TemplateScopeOrg}:                      true,
		{Scope: types.TemplateScopePerson, Person: aliceSub}: false,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/templates", nil)
		got := srv.templateMemberDrivesRefused(w, r, types.RunTemplate{Owner: scope, Document: doc})
		if got != refused {
			t.Errorf("%s: refused = %v, want %v (%s)", scope.Scope, got, refused, w.Body)
			continue
		}
		if !refused {
			continue
		}
		var body client.TemplateSaveRefusal
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Diagnostics) != 2 || body.Reason != reasonTemplateSharedFieldRefused ||
			body.Diagnostics[0].Path != "intent.drives[1]" || body.Diagnostics[1].Path != "intent.start_folder.drive_id" {
			t.Errorf("%s: refusal = %s", scope.Scope, w.Body)
		}
		if strings.Contains(w.Body.String(), mine.Name) {
			t.Errorf("%s: the refusal names the person's drive: %s", scope.Scope, w.Body)
		}
	}
	srv.cfg.Store = nil
	w := httptest.NewRecorder()
	if !srv.templateMemberDrivesRefused(w, httptest.NewRequest(http.MethodPost, "/api/v1/templates", nil),
		types.RunTemplate{Owner: types.TemplateOwner{Scope: types.TemplateScopeOrg}, Document: doc}) || w.Code != http.StatusInternalServerError {
		t.Errorf("no store: %d %s, want the save refused", w.Code, w.Body)
	}
}

// cancellingReclaim cancels the delete request while the substrate works, then
// answers outcome and err.
type cancellingReclaim struct {
	*memberDriveRunner
	cancel  context.CancelFunc
	outcome runner.DriveReclaimOutcome
	err     error
}

func (r *cancellingReclaim) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	r.cancel()
	return r.outcome, r.err
}

// ctxMemberDriveStore finishes and aborts a delete only on a live context, as
// a database call would.
type ctxMemberDriveStore struct{ *memberDriveStore }

func (s ctxMemberDriveStore) FinishMemberDriveDelete(ctx context.Context, id uuid.UUID, started time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memberDriveStore.FinishMemberDriveDelete(ctx, id, started)
}

func (s ctxMemberDriveStore) AbortMemberDriveDelete(ctx context.Context, id uuid.UUID, started time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memberDriveStore.AbortMemberDriveDelete(ctx, id, started)
}

// TestMemberDriveDeleteOutlivesTheRequest: a delete whose caller goes away
// mid-reclaim still finishes. A failed reclaim clears the tombstone, so the
// drive is the owner's again; a successful one removes the row.
func TestMemberDriveDeleteOutlivesTheRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome runner.DriveReclaimOutcome
		err     error
		kept    bool
	}{
		"reclaim fails": {err: context.Canceled, kept: true},
		"reclaim works": {outcome: runner.DriveReclaimDeleted},
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemberDriveStore()
			rn := &cancellingReclaim{memberDriveRunner: catalogueRunner(), outcome: tc.outcome, err: tc.err}
			srv, _ := memberDriveServer(st, rn, "docker")
			srv.cfg.Store = ctxMemberDriveStore{st}
			alice := personCtx(aliceSub)
			var made client.MeDrive
			if w := createMemberDrive(srv, alice, "Notes"); w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &made) != nil {
				t.Fatalf("create = %d %s", w.Code, w.Body)
			}
			ctx, cancel := context.WithCancel(alice)
			defer cancel()
			rn.cancel = cancel
			memberCall(ctx, srv.handleDeleteMyDrive, http.MethodDelete, "/api/v1/me/drives/"+made.ID, "", map[string]string{"id": made.ID})
			d, ok := st.drives[uuid.MustParse(made.ID)]
			switch {
			case ok != tc.kept:
				t.Fatalf("row kept = %v, want %v", ok, tc.kept)
			case tc.kept && d.DeleteStartedAt != nil:
				t.Fatal("the tombstone stayed set after the caller went away")
			case tc.kept && len(meDrivesOf(t, srv, alice).Drives) != 1:
				t.Error("the kept drive is not back in its owner's catalogue")
			}
		})
	}
}

// blockingLifecycle is a substrate whose lifecycle check answers only when its
// context ends.
type blockingLifecycle struct{ *memberDriveRunner }

func (blockingLifecycle) CanManageDrives(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestMemberDriveDeleteLifecycleCheckBounded: a lifecycle check that never
// answers is cut off at driveReclaimTimeout; the delete is refused and the
// tombstone cleared, so the drive is its owner's again.
func TestMemberDriveDeleteLifecycleCheckBounded(t *testing.T) {
	st := newMemberDriveStore()
	srv, _ := memberDriveServer(st, blockingLifecycle{catalogueRunner()}, "docker")
	d, err := st.CreateMemberDrive(context.Background(), types.UserDrive{Name: "Notes", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub}, 3)
	if err != nil {
		t.Fatal(err)
	}
	id := d.ID.String()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- memberCall(personCtx(aliceSub), srv.handleDeleteMyDrive, http.MethodDelete, "/api/v1/me/drives/"+id, "", map[string]string{"id": id})
	}()
	select {
	case w := <-done:
		wantReason(t, w, http.StatusUnprocessableEntity, reasonMemberDriveDeleteUnavailable)
	case <-time.After(driveReclaimTimeout + 15*time.Second):
		t.Fatal("the delete is still waiting on the lifecycle check past driveReclaimTimeout")
	}
	if got := st.drives[d.ID]; got.DeleteStartedAt != nil {
		t.Error("the tombstone stayed set after the lifecycle check ran out")
	}
}

// agreeStore lets a refused dispatch fail a run that is not stored.
type agreeStore struct{ *memberDriveStore }

func (agreeStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	return false, nil
}

// TestMemberDriveAttachAndDispatchAgree: for every caller kind, a member drive
// attach and the dispatch re-read agree. A person's session, their own API
// token and a delegated caller attach it and dispatch keeps it; a device or a
// service identity, whose runs are created as itself, cannot attach it and
// does not see it.
func TestMemberDriveAttachAndDispatchAgree(t *testing.T) {
	st := newMemberDriveStore()
	srv, _ := memberDriveServer(st, catalogueRunner(), "docker")
	srv.cfg.Store = agreeStore{st}
	alice := personCtx(aliceSub)
	d, err := st.CreateMemberDrive(context.Background(), types.UserDrive{Name: "Notes", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub}, 3)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ctx    context.Context
		attach bool
	}{
		"session":          {alice, true},
		"own api token":    {withAPITokenID(alice, uuid.New()), true},
		"delegated":        {audit.WithDelegation(alice, types.DelegationVia{}), true},
		"device":           {context.WithValue(alice, deviceCtxKey{}, types.Device{ID: uuid.New()}), false},
		"service identity": {withServiceIdentity(alice, "ci-bot", aliceSub), false},
	} {
		drives, f := authorizeDrives(srv, tc.ctx, drivesRequest(d.ID.String()))
		if (f == nil) != tc.attach {
			t.Errorf("%s: attach refusal = %+v, want attach %v", name, f, tc.attach)
		}
		if f != nil {
			if f.body.Reason != string(authz.ReasonDriveNotFound) || len(meDrivesOf(t, srv, tc.ctx).Drives) != 0 {
				t.Errorf("%s: refused as %q, or the catalogue still lists the drive", name, f.body.Reason)
			}
			continue
		}
		_, creator := actorFromContext(tc.ctx, "")
		if !srv.memberDrivesStillThere(tc.ctx, types.AgentRun{ID: uuid.New(), CreatedBy: creator}, memberDriveOwners(drives)) {
			t.Errorf("%s: attach admitted the drive but dispatch refused it", name)
		}
	}
}

// startingFirstStore records the run's state each time dispatch re-reads a member drive.
type startingFirstStore struct {
	*memberRecheckStore
	statesAtRead []types.RunState
}

func (s *startingFirstStore) GetMemberDriveOf(ctx context.Context, owner string, id uuid.UUID) (types.UserDrive, error) {
	s.statesAtRead = append(s.statesAtRead, s.State())
	return s.memberRecheckStore.GetMemberDriveOf(ctx, owner, id)
}

// TestMemberDriveRereadFollowsTheStartingClaim: dispatch claims PENDING->STARTING before it re-reads
// the run's member drives. A delete reads STARTING after its tombstone, so with this order either the
// re-read meets the tombstone or the delete meets the STARTING run; a re-read before the claim would
// let both miss.
func TestMemberDriveRereadFollowsTheStartingClaim(t *testing.T) {
	id := uuid.New()
	row := types.UserDrive{ID: id, Name: "Mine", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub}
	fr := &fakeRunner{}
	srv, st, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
	order := &startingFirstStore{memberRecheckStore: &memberRecheckStore{dispatchTestStore: st, drives: map[uuid.UUID]types.UserDrive{id: row}}}
	srv.cfg.Store = order
	run.Task, run.CreatedBy, st.run.CreatedBy = "", aliceSub, aliceSub
	mount := types.DriveMount{DriveID: id, Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-drive-x", DriveName: "Mine",
		HomeName: "d-x", Target: runner.DrivesRoot + "/mine", Enforcement: types.StorageEnforcementNone}
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest", Drives: []types.DriveMount{mount},
		MemberDrives: map[uuid.UUID]string{id: aliceSub},
	})
	if len(order.statesAtRead) == 0 {
		t.Fatal("dispatch never re-read the member drive")
	}
	for i, s := range order.statesAtRead {
		if s != types.RunStarting {
			t.Fatalf("re-read %d ran with the run %s, want STARTING: the claim must come first", i, s)
		}
	}
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want the drive mounted once", fr.createCalls)
	}
}
