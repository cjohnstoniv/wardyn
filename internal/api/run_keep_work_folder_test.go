// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// keepRunner is a substrate that mounts drives and can delete them.
type keepRunner struct{ memberDriveRunner }

func (keepRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{UserDrives: true, Driver: "docker"}, nil
}

// keepStore records the run a kept drive could not be read for as failed.
type keepStore struct {
	*memberDriveStore
	failed []uuid.UUID
}

func (s *keepStore) UpdateRunStateIf(_ context.Context, id uuid.UUID, _, to types.RunState) (bool, error) {
	if to == types.RunFailed {
		s.failed = append(s.failed, id)
	}
	return false, nil
}

func keepSite(memberOn, keepOn bool, limit, sizeMiB int) types.SiteConfig {
	sc := memberDriveSite(limit, sizeMiB)
	m := sc.WorkspaceProviders.Storage.UserDrive.MemberCreate
	m.Enabled, m.KeepMainDrive = memberOn, keepOn
	return sc
}

func keepRequest(ctx context.Context) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
}

func keepGates(t *testing.T, srv *Server, ctx context.Context, spec types.RunPolicySpec) (*keptWorkFolder, *runRefusal) {
	t.Helper()
	return srv.keepWorkFolderGates(keepRequest(ctx), spec, false)
}

// TestKeepWorkFolderNeedsMemberDrivesAndItsSwitch: keep_work_folder is refused
// with its own reason unless member drives are allowed AND keep_main_drive is on.
func TestKeepWorkFolderNeedsMemberDrivesAndItsSwitch(t *testing.T) {
	drivesOff := keepSite(true, true, 3, 0)
	drivesOff.WorkspaceProviders.Storage.UserDrive.Disabled = true
	noSwitch := keepSite(true, true, 3, 0)
	noSwitch.WorkspaceProviders.Storage.UserDrive.MemberCreate = nil
	for name, sc := range map[string]types.SiteConfig{
		"member drives off, keep on": keepSite(false, true, 3, 0),
		"member drives on, keep off": keepSite(true, false, 3, 0),
		"both off":                   keepSite(false, false, 3, 0),
		"no member_create block":     noSwitch,
		"drives disabled":            drivesOff,
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemberDriveStore()
			st.site = sc
			srv, _ := memberDriveServer(st, &keepRunner{}, "docker")
			k, refusal := keepGates(t, srv, personCtx(aliceSub), types.RunPolicySpec{})
			if k != nil || refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != reasonKeepWorkFolderDisabled {
				t.Fatalf("gates = %+v, %+v; want 422 %s", k, refusal, reasonKeepWorkFolderDisabled)
			}
		})
	}
	st := newMemberDriveStore()
	st.site = keepSite(true, true, 3, 0)
	srv, _ := memberDriveServer(st, &keepRunner{}, "docker")
	if k, refusal := keepGates(t, srv, personCtx(aliceSub), types.RunPolicySpec{}); refusal != nil || k == nil || k.owner != aliceSub {
		t.Fatalf("both on: gates = %+v, %+v; want the drive for %s", k, refusal, aliceSub)
	}
}

// TestKeepWorkFolderOwnedByTheRunsPersonOnly: the kept drive is the run's
// person's, read back only under them; a caller that is not a person
// launching for themselves is refused.
func TestKeepWorkFolderOwnedByTheRunsPersonOnly(t *testing.T) {
	st := newMemberDriveStore()
	st.site = keepSite(true, true, 3, 0)
	ks := &keepStore{memberDriveStore: st}
	rec := &recRecorder{}
	srv := New(Config{BaseCtx: context.Background(), Store: ks, Audit: rec, Runner: &keepRunner{}, RunnerTarget: "docker"})
	ctx := personCtx(aliceSub)
	k, refusal := keepGates(t, srv, ctx, types.RunPolicySpec{})
	if refusal != nil {
		t.Fatalf("gates refused: %+v", refusal)
	}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub, Title: "nightly tests", State: types.RunPending}
	if refusal := srv.keepWorkFolder(keepRequest(ctx), k, run); refusal != nil {
		t.Fatalf("keep refused: %+v", refusal)
	}
	mine := st.owned(aliceSub)
	if len(mine) != 1 || mine[0].ID != keptWorkFolderDriveID(run.ID) || mine[0].OwnerSubject != aliceSub {
		t.Fatalf("alice's drives = %+v, want the run's kept drive", mine)
	}
	if len(st.owned(bobSub)) != 0 {
		t.Fatalf("bob owns a drive")
	}
	if data := driveAuditData(t, rec, "drive.member.create"); data["from_run"] != run.ID.String() || data["target"] != runner.ScratchWorkPath {
		t.Errorf("drive.member.create data = %v", data)
	}
	mount, id, owner, ok := srv.keptWorkFolderMount(context.Background(), run)
	if !ok || id != mine[0].ID || owner != aliceSub || mount.SubjectHash != types.DriveSubjectHash(aliceSub) {
		t.Fatalf("mount = %+v, %s, %q, %v; want alice's drive", mount, id, owner, ok)
	}
	// The same drive id under a run of someone else's is not theirs to mount.
	other := run
	other.CreatedBy = bobSub
	if _, _, _, ok := srv.keptWorkFolderMount(context.Background(), other); ok || len(ks.failed) != 1 {
		t.Fatalf("a run of bob's mounted alice's kept drive, or was not failed (failed %v)", ks.failed)
	}
	for name, c := range map[string]context.Context{
		"no person": context.Background(),
		"a service identity sponsored by the person": withServiceIdentity(personCtx(aliceSub), "ci-bot", aliceSub),
	} {
		if _, refusal := keepGates(t, srv, c, types.RunPolicySpec{}); refusal == nil || refusal.body.Reason != reasonMemberDriveRequiresPerson {
			t.Errorf("%s: gates = %+v, want %s", name, refusal, reasonMemberDriveRequiresPerson)
		}
	}
}

// TestKeepWorkFolderCountsTowardTheLimit: a person at the limit is refused at
// the gate and at the insert, and the kept drive counts like any other.
func TestKeepWorkFolderCountsTowardTheLimit(t *testing.T) {
	st := newMemberDriveStore()
	st.site = keepSite(true, true, 1, 0)
	srv, _ := memberDriveServer(st, &keepRunner{}, "docker")
	ctx := personCtx(aliceSub)
	k, refusal := keepGates(t, srv, ctx, types.RunPolicySpec{})
	if refusal != nil {
		t.Fatalf("gates refused under the limit: %+v", refusal)
	}
	if refusal := srv.keepWorkFolder(keepRequest(ctx), k, types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub}); refusal != nil {
		t.Fatalf("keep refused: %+v", refusal)
	}
	// The board's sentence for a keep launch, never the generic "Delete one to create another."
	const atLimit = "You have 1 of 1 drives. Delete one to keep this run's work folder."
	if _, refusal := keepGates(t, srv, ctx, types.RunPolicySpec{}); refusal == nil || refusal.body.Reason != reasonMemberDriveLimitReached || refusal.body.Error != atLimit {
		t.Fatalf("gates at the limit = %+v, want %s %q", refusal, reasonMemberDriveLimitReached, atLimit)
	}
	if refusal := srv.keepWorkFolder(keepRequest(ctx), k, types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub}); refusal == nil || refusal.body.Reason != reasonMemberDriveLimitReached || refusal.body.Error != atLimit {
		t.Fatalf("insert at the limit = %+v, want %s %q", refusal, reasonMemberDriveLimitReached, atLimit)
	}
	if got := srv.memberDriveCreateState(keepRequest(ctx)); got.Count != 1 || got.Limit != 1 {
		t.Errorf("catalogue count = %d of %d, want 1 of 1", got.Count, got.Limit)
	}
}

// TestKeepWorkFolderKeepsOnlyTheWorkFolder: the kept drive mounts at the work
// folder and nowhere else, and a mount over the work folder refuses the keep.
func TestKeepWorkFolderKeepsOnlyTheWorkFolder(t *testing.T) {
	st := newMemberDriveStore()
	st.site = keepSite(true, true, 3, 0)
	srv, _ := memberDriveServer(st, &keepRunner{}, "docker")
	ctx := personCtx(aliceSub)
	k, _ := keepGates(t, srv, ctx, types.RunPolicySpec{})
	run := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub, State: types.RunPending}
	if refusal := srv.keepWorkFolder(keepRequest(ctx), k, run); refusal != nil {
		t.Fatalf("keep refused: %+v", refusal)
	}
	mount, _, _, ok := srv.keptWorkFolderMount(context.Background(), run)
	if !ok || mount.Target != "/home/agent/work" || mount.ReadOnly {
		t.Fatalf("mount = %+v, want a writable mount at /home/agent/work only", mount)
	}
	for _, target := range []string{"/home/agent/work", "/home/agent/work/", "/home/agent"} {
		spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/x", Target: target}}}
		if _, refusal := keepGates(t, srv, ctx, spec); refusal == nil || refusal.body.Reason != reasonWorkspaceTargetOverlap {
			t.Errorf("mount at %q: gates = %+v, want %s", target, refusal, reasonWorkspaceTargetOverlap)
		}
	}
	spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/x", Target: "/home/agent/work/repo"}}}
	if _, refusal := keepGates(t, srv, ctx, spec); refusal != nil {
		t.Errorf("a mount inside the work folder refused the keep: %+v", refusal)
	}
}

// TestKeepWorkFolderSizeNotEnforcedOnDocker: the catalogue says a kept drive's
// size is not enforced on Docker, and is a claim request on Kubernetes.
func TestKeepWorkFolderSizeNotEnforcedOnDocker(t *testing.T) {
	for target, want := range map[string]types.StorageEnforcement{"docker": types.StorageEnforcementNone, "k8s": types.StorageEnforcementRequest} {
		st := newMemberDriveStore()
		size := 0
		if target == "k8s" {
			size = 1024
		}
		st.site = keepSite(true, true, 3, size)
		srv, _ := memberDriveServer(st, &keepRunner{}, target)
		got := srv.memberDriveCreateState(keepRequest(personCtx(aliceSub)))
		if got == nil || !got.KeepWorkFolder || got.Enforcement != want {
			t.Errorf("%s: create block = %+v, want keep_work_folder and enforcement %q", target, got, want)
		}
	}
	st := newMemberDriveStore()
	st.site = keepSite(true, false, 3, 0)
	srv, _ := memberDriveServer(st, &keepRunner{}, "docker")
	if got := srv.memberDriveCreateState(keepRequest(personCtx(aliceSub))); got.KeepWorkFolder {
		t.Errorf("keep_main_drive off: create block = %+v, want keep_work_folder false", got)
	}
}

// TestKeepWorkFolderSwitchReportedWhenTheDoorIsClosed: the catalogue's
// create.keep_work_folder is the keep_main_drive switch itself, so a screen
// tells "switch on, door closed" (at the limit, member drives off) from
// "switch off"; the closing reason still travels beside it.
func TestKeepWorkFolderSwitchReportedWhenTheDoorIsClosed(t *testing.T) {
	ctx := personCtx(aliceSub)
	for name, c := range map[string]struct {
		site   types.SiteConfig
		atMax  bool
		keep   bool
		reason client.MemberDriveCreateReason
	}{
		"switch on, at the limit":       {keepSite(true, true, 1, 0), true, true, client.MemberDriveCreateLimitReached},
		"switch on, member drives off":  {keepSite(false, true, 1, 0), false, true, client.MemberDriveCreateDisabled},
		"switch off, member drives on":  {keepSite(true, false, 1, 0), false, false, ""},
		"switch off, member drives off": {keepSite(false, false, 1, 0), false, false, client.MemberDriveCreateDisabled},
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemberDriveStore()
			st.site = c.site
			srv, _ := memberDriveServer(st, &keepRunner{}, "docker")
			if c.atMax {
				if _, err := st.CreateMemberDrive(ctx, types.UserDrive{Name: "mine", OwnerSubject: aliceSub, Backend: types.DriveBackendDockerVolume}, 1); err != nil {
					t.Fatal(err)
				}
			}
			got := srv.memberDriveCreateState(keepRequest(ctx))
			if got == nil || got.KeepWorkFolder != c.keep || got.Reason != c.reason {
				t.Fatalf("create block = %+v, want keep_work_folder %v reason %q", got, c.keep, c.reason)
			}
			if c.atMax && (got.Count != 1 || got.Limit != 1) {
				t.Errorf("at the limit: create block = %+v, want 1 of 1", got)
			}
		})
	}
}

// TestKeepWorkFolderMarksTheDriveWithItsRun: the kept drive's catalogue row
// carries kept_from_run, the run's id; a drive made by hand carries none.
func TestKeepWorkFolderMarksTheDriveWithItsRun(t *testing.T) {
	st := newMemberDriveStore()
	st.site = keepSite(true, true, 3, 0)
	srv, _ := memberDriveServer(st, &keepRunner{}, "docker")
	ctx := personCtx(aliceSub)
	k, refusal := keepGates(t, srv, ctx, types.RunPolicySpec{})
	if refusal != nil {
		t.Fatalf("gates refused: %+v", refusal)
	}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub, Title: "nightly tests"}
	if refusal := srv.keepWorkFolder(keepRequest(ctx), k, run); refusal != nil {
		t.Fatalf("keep refused: %+v", refusal)
	}
	if _, err := st.CreateMemberDrive(ctx, types.UserDrive{Name: "by hand", OwnerSubject: aliceSub, Backend: types.DriveBackendDockerVolume}, 3); err != nil {
		t.Fatal(err)
	}
	marks := map[string]string{}
	for _, d := range st.owned(aliceSub) {
		marks[d.Name] = memberMeDrive(d).KeptFromRun
	}
	if marks[keptWorkFolderName(run)] != run.ID.String() || marks["by hand"] != "" || len(marks) != 2 {
		t.Fatalf("kept_from_run by drive = %v, want the run id on the kept drive only", marks)
	}
}

// TestKeptDriveCatalogueNamesItsRun: the catalogue row of a kept drive carries
// the run's id and its title as the catalogue is read; a run that is gone, or
// one that is not the owner's, leaves the title out, and a drive made by hand
// carries neither.
func TestKeptDriveCatalogueNamesItsRun(t *testing.T) {
	st := newMemberDriveStore()
	st.site = keepSite(true, true, 5, 0)
	srv, _ := memberDriveServer(st, catalogueRunner(), "docker")
	ctx := personCtx(aliceSub)
	mk := func(name string, run *types.AgentRun) {
		d := types.UserDrive{Name: name, OwnerSubject: aliceSub, Backend: types.DriveBackendDockerVolume}
		if run != nil {
			d.KeptFromRun = &run.ID
		}
		if _, err := st.CreateMemberDrive(ctx, d, 5); err != nil {
			t.Fatal(err)
		}
	}
	live := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub, Title: "nightly-tests"}
	gone := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub, Title: "never stored"}
	foreign := types.AgentRun{ID: uuid.New(), CreatedBy: bobSub, Title: "bob's run"}
	st.runs = map[uuid.UUID]types.AgentRun{live.ID: live, foreign.ID: foreign}
	mk("kept live", &live)
	mk("kept gone", &gone)
	mk("kept foreign", &foreign)
	mk("by hand", nil)

	type kept struct{ run, title string }
	got := map[string]kept{}
	for _, d := range meDrivesOf(t, srv, ctx).Drives {
		got[d.Name] = kept{d.KeptFromRun, d.KeptFromRunTitle}
	}
	want := map[string]kept{
		"kept live":    {live.ID.String(), "nightly-tests"},
		"kept gone":    {gone.ID.String(), ""},
		"kept foreign": {foreign.ID.String(), ""},
		"by hand":      {"", ""},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: kept_from_run, kept_from_run_title = %+v, want %+v", name, got[name], w)
		}
	}
}

// TestKeepWorkFolderHoldsNoDeliveredSecret: a file secret delivered to a run
// that keeps its work folder lands in runner.ComponentSecretDir, outside every
// drive the run mounts, the kept one included; and no managed file may sit
// inside the work folder at all.
func TestKeepWorkFolderHoldsNoDeliveredSecret(t *testing.T) {
	rn := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	run.Task, run.CreatedBy = "", fileOwner
	st.run = run
	_, sec, _ := fileSecretServer(t)
	srv.cfg.Secrets, srv.cfg.MaskRegistry = sec, secretmask.NewRegistry()
	kept, err := memberDriveMount(types.UserDrive{ID: keptWorkFolderDriveID(run.ID), Name: "Run kept", Backend: types.DriveBackendDockerVolume, OwnerSubject: fileOwner})
	if err != nil {
		t.Fatal(err)
	}
	kept.Target = runner.ScratchWorkPath
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest", Drives: []types.DriveMount{kept},
		Policy: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{fileSecretGrant("api-token", "deploy-token", true)}},
	})
	if rn.createCalls != 1 {
		t.Fatalf("the sandbox was not created; events: %s", auditDump(audit.events, run.ID))
	}
	spec := rn.lastSpec
	var keptThere, delivered bool
	for _, d := range spec.Drives {
		keptThere = keptThere || d.Target == runner.ScratchWorkPath
	}
	for _, f := range spec.ManagedFiles {
		delivered = delivered || f.AgentOwned && string(f.Content) == fileOwnValue
		for _, d := range spec.Drives {
			if f.Path == d.Target || strings.HasPrefix(f.Path, strings.TrimSuffix(d.Target, "/")+"/") {
				t.Errorf("managed file %s sits on the drive mounted at %s", f.Path, d.Target)
			}
		}
	}
	if !keptThere || !delivered {
		t.Fatalf("spec drives %+v, file delivered %v; want the kept drive at %s and the file secret", spec.Drives, delivered, runner.ScratchWorkPath)
	}
	inside := runner.ManagedFile{Path: runner.ScratchWorkPath + "/api-token", Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: []byte("x")}
	if err := runner.ValidateManagedFiles([]runner.ManagedFile{inside}); err == nil {
		t.Errorf("a delivered secret at %s was admitted", inside.Path)
	}
}
