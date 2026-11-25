// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMoveDrives_ReattachOnlyTheSameStorage: the recorded work drive is never re-attached; another drive is
// re-attached only where the destination mounts the same storage, and refuses with layer drive anywhere else.
func TestMoveDrives_ReattachOnlyTheSameStorage(t *testing.T) {
	rid, other := uuid.New(), uuid.New()
	work := types.DriveMount{DriveName: "work", Backend: types.DriveBackendDockerVolume, Target: runner.ScratchWorkPath}
	notes := types.DriveMount{DriveName: "notes", Backend: types.DriveBackendDockerVolume, Target: "/home/agent/drives/notes"}
	share := types.DriveMount{DriveName: "share", Backend: types.DriveBackendHostPath, Target: "/home/agent/drives/share", RunnerID: &rid}
	team := types.DriveMount{DriveName: "team-notes", Backend: types.DriveBackendDockerVolume, Target: "/home/agent/shared/team", Team: true}
	org := moveDest{Placement: types.PlacementRemote}
	onRunner := moveDest{Placement: types.PlacementLocal, RunnerID: &rid}
	onOther := moveDest{Placement: types.PlacementLocal, RunnerID: &other}
	orgRun, runnerRun := types.AgentRun{}, types.AgentRun{RunnerID: &other}

	for _, c := range []struct {
		name      string
		drives    []types.DriveMount
		run       types.AgentRun
		d         moveDest
		orgTarget string
		want      []string // drive names re-attached; nil with refused set
		refused   string   // a word of the refusal's sentence
	}{
		{"org to org drops the work drive and keeps the rest", []types.DriveMount{work, notes}, orgRun, org, "docker", []string{"notes"}, ""},
		{"a docker volume at a Kubernetes organisation", []types.DriveMount{notes}, orgRun, org, "k8s", nil, "same storage"},
		{"a docker volume to a runner", []types.DriveMount{work, notes}, orgRun, onRunner, "docker", nil, "same storage"},
		{"a drive bound to the destination runner", []types.DriveMount{work, share}, runnerRun, onRunner, "docker", []string{"share"}, ""},
		{"a drive bound to another runner", []types.DriveMount{share}, orgRun, onOther, "docker", nil, "same storage"},
		{"a runner's bound drive to the organisation", []types.DriveMount{share}, runnerRun, org, "docker", nil, "same storage"},
		{"a team drive to a runner", []types.DriveMount{team}, orgRun, onRunner, "docker", nil, "shared or read-only"},
		{"only the work drive", []types.DriveMount{work}, orgRun, onRunner, "docker", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ref := moveDestDrives(runSandboxSpecRecord{Drives: c.drives}, c.run, c.d, c.orgTarget)
			if c.refused != "" {
				if ref == nil || ref.layer != "drive" || ref.reason != reasonMoveDestinationRefused || !strings.Contains(ref.msg, c.refused) {
					t.Fatalf("refusal = %+v, want layer drive naming %q", ref, c.refused)
				}
				return
			}
			if ref != nil {
				t.Fatalf("refused: %+v", ref)
			}
			var names []string
			for _, dr := range got {
				names = append(names, dr.DriveName)
			}
			if !slices.Equal(names, c.want) {
				t.Errorf("re-attached %v, want %v", names, c.want)
			}
		})
	}
	// moveRecordRefusal answers the same refusal.
	caps := runner.Capabilities{ConfinementClasses: []types.ConfinementClass{types.CC1}}
	if ref := moveRecordRefusal(runSandboxSpecRecord{Drives: []types.DriveMount{notes}}, orgRun, onRunner, caps, "docker"); ref == nil || ref.layer != "drive" {
		t.Errorf("moveRecordRefusal = %+v, want layer drive", ref)
	}
}

// TestMoveDrives_KeepJudgesTheDestinationsDrives: the destination's keep is asked about the drives the
// destination's spec mounts, the new work drive included, never the record's. A Kubernetes destination sees a cluster
// claim at the work folder in place of a Docker source's volume; the source's keep is asked about the record.
func TestMoveDrives_KeepJudgesTheDestinationsDrives(t *testing.T) {
	notes := types.DriveMount{DriveName: "notes", Backend: types.DriveBackendK8sPVC, Target: "/home/agent/drives/notes"}
	got := moveKeepDrives(moveDestPlan{Drives: []types.DriveMount{notes}, WorkDrive: true})
	if len(got) != 2 || got[0].DriveName != "notes" || got[1].Target != runner.ScratchWorkPath || got[1].Backend != types.DriveBackendK8sPVC {
		t.Errorf("keep drives = %+v, want notes and a cluster claim at the work folder", got)
	}
	if got := moveKeepDrives(moveDestPlan{Drives: []types.DriveMount{notes}}); len(got) != 1 {
		t.Errorf("keep drives without a work drive = %+v, want notes alone", got)
	}

	f := newMoveFixture(t)
	f.srv.cfg.RunnerTarget = "k8s"
	srcWork := types.DriveMount{DriveName: "laptop work", Backend: types.DriveBackendDockerVolume, Target: runner.ScratchWorkPath}
	f.record(t, runSandboxSpecRecord{Image: "img", ConfinementClass: types.CC1, Interactive: true, Drives: []types.DriveMount{srcWork}})
	var mu sync.Mutex
	var asked [][]types.DriveMount
	movePortStopRefusal = func(_ context.Context, a moveKeepArgs) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, slices.Clone(a.Drives))
		return "", nil
	}
	code, body := f.move(t, "org")
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 {
		t.Fatalf("the keep was asked %d times, want the source then the destination", len(asked))
	}
	if len(asked[0]) != 1 || asked[0][0].DriveName != "laptop work" {
		t.Errorf("the source's keep saw %+v, want the recorded work drive", asked[0])
	}
	if dest := asked[1]; len(dest) != 1 || dest[0].Target != runner.ScratchWorkPath || dest[0].Backend != types.DriveBackendK8sPVC {
		t.Errorf("the destination's keep saw %+v, want one cluster claim at the work folder", dest)
	}
	// The admin token is no person, so the new work drive's first door refuses the move before anything stops.
	reason, field := moveAnswer(t, body)
	if code != http.StatusForbidden || reason != reasonMoveWorkDriveRefused || field != reasonMemberDriveRequiresPerson {
		t.Errorf("move = %d %s, want 403 %s layer %s", code, body, reasonMoveWorkDriveRefused, reasonMemberDriveRequiresPerson)
	}
	f.wantNotStopped(t)
}

// TestMoveDrives_ReattachedDrivesAreNotCarried: every re-attached drive's target is left out of the carried set,
// and the port's own set is not changed.
func TestMoveDrives_ReattachedDrivesAreNotCarried(t *testing.T) {
	set := runner.CarriedSet{Roots: []string{"/home/agent/work", "/home/agent/drives/notes"}, Exclude: []string{"home/agent/work/.cache"}}
	got := moveExcludeReattached(set, []types.DriveMount{{Target: "/home/agent/drives/notes"}})
	if !slices.Equal(got.Exclude, []string{"home/agent/work/.cache", "home/agent/drives/notes"}) || !slices.Equal(got.Roots, set.Roots) {
		t.Errorf("set = %+v, want the drive's target excluded and the roots kept", got)
	}
	if len(set.Exclude) != 1 {
		t.Errorf("the port's set changed: %+v", set)
	}
}

// TestMoveWorkDrive_IDFromRunAndMove: the work drive's id is derived from the run and the move: the same pair gives
// the same id, and it is never the run's own kept drive.
func TestMoveWorkDrive_IDFromRunAndMove(t *testing.T) {
	run, mv := uuid.New(), uuid.New()
	if moveWorkDriveID(run, mv) != moveWorkDriveID(run, mv) {
		t.Error("the same run and move give two ids")
	}
	if moveWorkDriveID(run, mv) == moveWorkDriveID(run, uuid.New()) {
		t.Error("two moves of one run give one id")
	}
	if moveWorkDriveID(run, mv) == keptWorkFolderDriveID(run) {
		t.Error("the move's work drive is the run's own kept drive")
	}
}

// moveDriveServer is a Kubernetes deployment with member drives and keep_work_folder on, whose substrate confirms
// every delete.
func moveDriveServer(t *testing.T, keepOn bool, target string) (*Server, *memberDriveStore, *recRecorder) {
	t.Helper()
	st := newMemberDriveStore()
	st.site = keepSite(true, keepOn, 3, 512)
	srv, rec := memberDriveServer(st, &keepRunner{memberDriveRunner{reclaimRunner: reclaimRunner{Runner: &fakeRunner{},
		outcome: runner.DriveReclaimDeleted}}}, target)
	return srv, st, rec
}

// TestMoveWorkDrive_DoorsAreKeepWorkFolders: the new work drive meets keep_work_folder's doors as the run's person,
// and each refusal is move_work_drive_refused with the door's reason as its layer.
func TestMoveWorkDrive_DoorsAreKeepWorkFolders(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub}
	org := moveDest{Placement: types.PlacementRemote}
	rid := uuid.New()
	srv, _, _ := moveDriveServer(t, true, "k8s")
	k, ref, err := srv.moveWorkDriveDoors(personCtx(aliceSub), run, org, types.RunPolicySpec{})
	if err != nil || ref != nil || k == nil || k.row.Backend != types.DriveBackendK8sPVC || k.row.SizeMiB != 512 || k.owner != aliceSub {
		t.Fatalf("doors = %+v, %+v, %v; want alice's cluster claim of 512 MiB", k, ref, err)
	}
	off, _, _ := moveDriveServer(t, false, "k8s")
	docker, _, _ := moveDriveServer(t, true, "docker")
	for _, c := range []struct {
		name  string
		srv   *Server
		ctx   context.Context
		d     moveDest
		layer string
	}{
		{"another person moves the run", srv, personCtx(bobSub), org, reasonMemberDriveRequiresPerson},
		{"keep_work_folder is off", off, personCtx(aliceSub), org, reasonKeepWorkFolderDisabled},
		{"the deployment's drives are Docker volumes", docker, personCtx(aliceSub), org, reasonMemberDriveUnsupported},
		{"a person's runner", srv, personCtx(aliceSub), moveDest{Placement: types.PlacementLocal, RunnerID: &rid}, driveRefusalBackendElsewhere},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, ref, err := c.srv.moveWorkDriveDoors(c.ctx, run, c.d, types.RunPolicySpec{})
			if err != nil || k != nil || ref == nil || ref.reason != reasonMoveWorkDriveRefused || ref.layer != c.layer || ref.msg == "" {
				t.Fatalf("doors = %+v, %+v, %v; want %s layer %s", k, ref, err, reasonMoveWorkDriveRefused, c.layer)
			}
		})
	}
}

// TestMoveWorkDrive_MadeThenDeletedWhenTheMoveFails: the move makes the drive the doors admitted, marked with the
// run, at least as large as the source's work drive, mounted at the work folder; a failed move's cleanup deletes it
// through a person's own delete, and a second cleanup finds nothing to do.
func TestMoveWorkDrive_MadeThenDeletedWhenTheMoveFails(t *testing.T) {
	srv, st, rec := moveDriveServer(t, true, "k8s")
	run := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub, Title: "nightly tests"}
	k, ref, err := srv.moveWorkDriveDoors(personCtx(aliceSub), run, moveDest{Placement: types.PlacementRemote}, types.RunPolicySpec{})
	if err != nil || ref != nil {
		t.Fatalf("doors: %+v, %v", ref, err)
	}
	mv := types.RunMove{ID: uuid.New(), RunID: run.ID}
	x := &moveExec{s: srv, mv: mv, run: run, work: k, actorType: types.ActorHuman, principal: aliceSub,
		rec: runSandboxSpecRecord{Drives: []types.DriveMount{{Target: runner.ScratchWorkPath, SizeMiB: 2048}}}}
	if end := x.makeWorkDrive(context.Background()); end != nil {
		t.Fatalf("make: %+v", end)
	}
	mine := st.owned(aliceSub)
	if len(mine) != 1 || mine[0].ID != moveWorkDriveID(run.ID, mv.ID) || mine[0].KeptFromRun == nil || *mine[0].KeptFromRun != run.ID ||
		mine[0].SizeMiB != 2048 || mine[0].Name == keptWorkFolderName(run) {
		t.Fatalf("alice's drives = %+v, want the move's drive of 2048 MiB marked with the run", mine)
	}
	if m := x.workMount; m == nil || m.Target != runner.ScratchWorkPath || m.Backend != types.DriveBackendK8sPVC || m.DriveID != mine[0].ID {
		t.Fatalf("mount = %+v, want the drive at the work folder", m)
	}
	if data := driveAuditData(t, rec, "drive.member.create"); data["from_run"] != run.ID.String() || data["target"] != runner.ScratchWorkPath {
		t.Errorf("drive.member.create data = %v", data)
	}
	if !srv.dropMoveWorkDrive(context.Background(), run.ID, mv.ID) {
		t.Fatal("the failed move's drive was not deleted")
	}
	if left := st.owned(aliceSub); len(left) != 0 {
		t.Fatalf("alice's drives after the cleanup = %+v, want none", left)
	}
	if data := driveAuditData(t, rec, "drive.member.delete"); data["by"] != memberDriveByMoveFailed {
		t.Errorf("drive.member.delete data = %v, want by %s", data, memberDriveByMoveFailed)
	}
	if !srv.dropMoveWorkDrive(context.Background(), run.ID, mv.ID) {
		t.Error("a second cleanup did not find the drive gone")
	}
}
