// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// movedWorkDrive is a Kubernetes deployment where a move of alice's running run has made the run's new work drive,
// and that move's executor, whose row ms holds as importing.
func movedWorkDrive(t *testing.T) (*memberDriveStore, *moveExec, *moveStore) {
	t.Helper()
	srv, st, _ := moveDriveServer(t, true, "k8s")
	run := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub, Title: "nightly tests", State: types.RunRunning}
	st.runs = map[uuid.UUID]types.AgentRun{run.ID: run}
	k, ref, err := srv.moveWorkDriveDoors(personCtx(aliceSub), run, moveDest{Placement: types.PlacementRemote}, types.RunPolicySpec{}, 0)
	if err != nil || ref != nil {
		t.Fatalf("doors: %+v, %v", ref, err)
	}
	mv := types.RunMove{ID: uuid.New(), RunID: run.ID, State: types.MoveImporting, CreatedAt: time.Now()}
	row := mv
	ms := &moveStore{moves: []*types.RunMove{&row}}
	x := &moveExec{s: srv, ms: ms, mv: mv, run: run, work: k, actorType: types.ActorHuman, principal: aliceSub}
	if end := x.makeWorkDrive(context.Background()); end != nil {
		t.Fatalf("make: %+v", end)
	}
	if mine := st.owned(aliceSub); len(mine) != 1 {
		t.Fatalf("alice's drives = %+v, want the move's", mine)
	}
	return st, x, ms
}

func moveRowOf(ms *moveStore, id uuid.UUID) types.RunMove {
	ms.mmu.Lock()
	defer ms.mmu.Unlock()
	return *ms.find(id)
}

// TestMoveWorkDrive_EndDropsIt: a move that fails after it made the work drive deletes the drive as it ends, and
// records its cleanup done.
func TestMoveWorkDrive_EndDropsIt(t *testing.T) {
	st, x, ms := movedWorkDrive(t)
	x.end(context.Background(), failMove(reasonMoveImportFailed, "The import failed."))
	if left := st.owned(aliceSub); len(left) != 0 {
		t.Errorf("alice's drives after the failed move = %+v, want none", left)
	}
	if row := moveRowOf(ms, x.mv.ID); row.State != types.MoveFailed || row.Cleanup != types.MoveCleanupDone {
		t.Errorf("move row %s cleanup %s, want failed with its cleanup done", row.State, row.Cleanup)
	}
}

// TestMoveWorkDrive_UndroppedLeavesCleanupPending: a work drive that cannot be deleted yet leaves the failed move's
// cleanup pending for the move sweep.
func TestMoveWorkDrive_UndroppedLeavesCleanupPending(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(st *memberDriveStore, x *moveExec)
	}{
		{"the owner has a run starting", func(st *memberDriveStore, _ *moveExec) { st.starting = map[string]bool{aliceSub: true} }},
		{"the substrate's delete fails", func(_ *memberDriveStore, x *moveExec) {
			x.s.cfg.Runner.(*keepRunner).err = errors.New("the claim is still bound")
		}},
		{"the run cannot be read", func(st *memberDriveStore, x *moveExec) { st.runs = nil; x.s.cfg.Store = runReadFails{st} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, x, ms := movedWorkDrive(t)
			c.setup(st, x)
			x.end(context.Background(), failMove(reasonMoveImportFailed, "The import failed."))
			if row := moveRowOf(ms, x.mv.ID); row.Cleanup != types.MoveCleanupPending {
				t.Errorf("move row cleanup %s, want pending", row.Cleanup)
			}
			if !slices.ContainsFunc(st.owned(aliceSub), func(d types.UserDrive) bool { return d.ID == moveWorkDriveID(x.run.ID, x.mv.ID) }) {
				t.Error("the work drive's row is gone though its delete did not finish")
			}
		})
	}
}

// runReadFails is the drive store whose run reads fail.
type runReadFails struct{ *memberDriveStore }

func (runReadFails) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, errors.New("the database is unreachable")
}

// TestMoveWorkDrive_SweepDropsIt: the move sweep deletes a failed move's work drive on both of its cleanup arms: the
// destination sandbox gone or live, and the one it left removed.
func TestMoveWorkDrive_SweepDropsIt(t *testing.T) {
	for _, c := range []struct{ name, ref string }{
		{"no destination sandbox was recorded", ""},
		{"the destination sandbox is removed", "ref-dest"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, x, ms := movedWorkDrive(t)
			ms.mmu.Lock()
			row := ms.find(x.mv.ID)
			row.State, row.Failure, row.Cleanup, row.ToSandboxRef = types.MoveFailed, reasonMoveImportFailed, types.MoveCleanupPending, c.ref
			due := *row
			ms.mmu.Unlock()
			x.s.sweepRunMove(context.Background(), ms, store.RunMoveDue{Move: due})
			if left := st.owned(aliceSub); len(left) != 0 {
				t.Errorf("alice's drives after the sweep = %+v, want none", left)
			}
			if got := moveRowOf(ms, x.mv.ID); got.Cleanup != types.MoveCleanupDone {
				t.Errorf("move row cleanup %s, want done", got.Cleanup)
			}
		})
	}
}

// TestMoveWorkDrive_LeftWhenNotTheMoves: the cleanup leaves, and counts as done, a drive under the move's id that
// another run keeps, and the move's own drive once its run has ended or is gone: that drive is then the person's.
func TestMoveWorkDrive_LeftWhenNotTheMoves(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(st *memberDriveStore, x *moveExec)
	}{
		{"another run keeps the drive", func(st *memberDriveStore, x *moveExec) {
			id, other := moveWorkDriveID(x.run.ID, x.mv.ID), uuid.New()
			d := st.drives[id]
			d.KeptFromRun = &other
			st.drives[id] = d
		}},
		{"the run has ended", func(st *memberDriveStore, x *moveExec) {
			run := st.runs[x.run.ID]
			run.State = types.RunCompleted
			st.runs[x.run.ID] = run
		}},
		{"the run is gone", func(st *memberDriveStore, x *moveExec) { delete(st.runs, x.run.ID) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, x, _ := movedWorkDrive(t)
			c.setup(st, x)
			if !x.s.dropMoveWorkDrive(context.Background(), x.run.ID, x.mv.ID) {
				t.Error("the cleanup is left pending, want it done")
			}
			if mine := st.owned(aliceSub); len(mine) != 1 {
				t.Errorf("alice's drives = %+v, want the drive left", mine)
			}
		})
	}
}

// TestMoveWorkDrive_PendingCleanupHoldsTheNextMove: a move whose sandbox or work drive is still being removed
// refuses the run's next move, and says what it waits for.
func TestMoveWorkDrive_PendingCleanupHoldsTheNextMove(t *testing.T) {
	runID := uuid.New()
	ms := &moveStore{moves: []*types.RunMove{{ID: uuid.New(), RunID: runID, State: types.MoveFailed, Failure: reasonMoveImportFailed,
		Cleanup: types.MoveCleanupPending, CreatedAt: time.Now()}}}
	ref, err := moveHeldRefusal(context.Background(), ms, runID)
	if err != nil || ref == nil || ref.reason != reasonMoveBusy || !strings.Contains(ref.msg, "work drive") {
		t.Errorf("refusal = %+v, %v; want %s naming the work drive", ref, err, reasonMoveBusy)
	}
}

// TestMoveWorkDrive_SizeCeiling: a source work drive larger than a new drive may be made is refused at admission
// with layer drive_size, and one at the ceiling is admitted.
func TestMoveWorkDrive_SizeCeiling(t *testing.T) {
	srv, st, _ := moveDriveServer(t, true, "k8s")
	st.site.WorkspaceProviders.Storage.UserDrive.MaxSizeMiB = 1024
	run := types.AgentRun{ID: uuid.New(), CreatedBy: aliceSub}
	org := moveDest{Placement: types.PlacementRemote}
	k, ref, err := srv.moveWorkDriveDoors(personCtx(aliceSub), run, org, types.RunPolicySpec{}, 2048)
	if err != nil || k != nil || ref == nil || ref.reason != reasonMoveWorkDriveRefused || ref.layer != moveLayerWorkDriveSize ||
		ref.status != http.StatusUnprocessableEntity || !strings.Contains(ref.msg, "1024 MiB") {
		t.Errorf("doors = %+v, %+v, %v; want %s layer %s naming the 1024 MiB ceiling", k, ref, err, reasonMoveWorkDriveRefused, moveLayerWorkDriveSize)
	}
	if k, ref, err := srv.moveWorkDriveDoors(personCtx(aliceSub), run, org, types.RunPolicySpec{}, 1024); err != nil || ref != nil || k == nil {
		t.Errorf("doors at the ceiling = %+v, %+v, %v; want admitted", k, ref, err)
	}
}

// TestMoveWorkDrive_NameKeepsBothIDsInTheSlug: a drive's object name carries its slug's first 40 characters, so the
// move's drive name leads with the run's and the move's ids: two moves of one run under a long title, and the run's
// own kept drive, fold to three slugs.
func TestMoveWorkDrive_NameKeepsBothIDsInTheSlug(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), Title: strings.Repeat("integration tests ", 4)[:60]}
	one, two := uuid.New(), uuid.New()
	slugs := []string{types.DriveSlug(keptWorkFolderName(run)), types.DriveSlug(moveWorkDriveName(run, one)), types.DriveSlug(moveWorkDriveName(run, two))}
	if slugs[0] == slugs[1] || slugs[0] == slugs[2] || slugs[1] == slugs[2] {
		t.Errorf("slugs %q, want three", slugs)
	}
	for i, mv := range []uuid.UUID{one, two} {
		if s := slugs[i+1]; !strings.Contains(s, run.ID.String()[:8]) || !strings.Contains(s, mv.String()[:8]) || len(s) > 40 {
			t.Errorf("slug %q, want the run's and the move's first blocks within 40 characters", s)
		}
	}
	if err := types.ValidateDriveName(moveWorkDriveName(run, one)); err != nil {
		t.Errorf("the name is not a drive name: %v", err)
	}
}

// TestMoveDrives_DestinationSpecIsReattachedAndNewWork: the destination's spec mounts the re-attached drives and the
// move's new work drive, never the recorded work drive.
func TestMoveDrives_DestinationSpecIsReattachedAndNewWork(t *testing.T) {
	oldWork := types.DriveMount{DriveName: "laptop work", Backend: types.DriveBackendDockerVolume, Target: runner.ScratchWorkPath}
	notes := types.DriveMount{DriveName: "notes", Backend: types.DriveBackendK8sPVC, Target: "/home/agent/drives/notes"}
	newWork := types.DriveMount{DriveName: "Moved", Backend: types.DriveBackendK8sPVC, Target: runner.ScratchWorkPath}
	x := &moveExec{rec: runSandboxSpecRecord{Drives: []types.DriveMount{oldWork, notes}}, plan: moveDestPlan{Drives: []types.DriveMount{notes}},
		dest: moveDest{Placement: types.PlacementRemote}}
	if got := x.destSpec().Drives; !slices.Equal(got, []types.DriveMount{notes}) {
		t.Errorf("drives without a new work drive = %+v, want notes alone", got)
	}
	x.workMount = &newWork
	if got := x.destSpec().Drives; !slices.Equal(got, []types.DriveMount{notes, newWork}) {
		t.Errorf("drives = %+v, want notes and the new work drive", got)
	}
}

// TestMoveDrives_MovedRecordAndCarriedSet: a move between the organisation's Docker executors re-attaches the
// run's drive, leaves its files out of what is carried, mounts no recorded work drive at the destination, and
// records the destination's drives as the run's.
func TestMoveDrives_MovedRecordAndCarriedSet(t *testing.T) {
	f := newMoveFixture(t)
	f.srv.cfg.RunnerTarget = "docker"
	work := types.DriveMount{DriveName: "work", Backend: types.DriveBackendDockerVolume, Target: runner.ScratchWorkPath}
	notes := types.DriveMount{DriveName: "notes", Backend: types.DriveBackendDockerVolume, Target: "/home/agent/drives/notes"}
	f.record(t, runSandboxSpecRecord{Image: "img", ConfinementClass: types.CC1, Interactive: true, Drives: []types.DriveMount{work, notes}})
	f.fk.mu.Lock()
	f.fk.carried = moveCarriedSet{Roots: []string{"/home/agent/work", "/home/agent/drives/notes"}}
	f.fk.mu.Unlock()
	f.mr.mmu.Lock()
	f.mr.files = append(f.mr.files, moveDir("home/agent/drives/notes"), moveReg("home/agent/drives/notes/todo.md", "x"))
	f.mr.mmu.Unlock()
	if code, body := f.move(t, "org"); code != http.StatusAccepted || f.only(t).State != types.MoveCommitted {
		t.Fatalf("move = %d %s, want 202 and committed", code, body)
	}
	if got := f.mr.lastImport().Drives; !slices.Equal(got, []types.DriveMount{notes}) {
		t.Errorf("destination drives = %+v, want notes alone", got)
	}
	f.mr.mmu.Lock()
	imported := slices.Clone(f.mr.imported)
	f.mr.mmu.Unlock()
	if !slices.Contains(imported, "home/agent/work/README.md") || slices.ContainsFunc(imported, func(p string) bool { return strings.HasPrefix(p, "home/agent/drives") }) {
		t.Errorf("carried %v, want the work folder and nothing of the re-attached drive", imported)
	}
	rec, err := f.srv.loadRunSandboxSpec(context.Background(), f.run.ID)
	if err != nil || !slices.Equal(rec.Drives, []types.DriveMount{notes}) || rec.Image != "img" {
		t.Errorf("record after the move = %+v, %v; want the destination's drives and the rest kept", rec, err)
	}
}

// TestMoveDrives_SecondMoveKeepsOnTheNewWorkDrive: after a move from Docker to Kubernetes the run's record mounts
// the move's new work drive, so a second move's keep is judged on that cluster claim, not the Docker volume the
// run started with.
func TestMoveDrives_SecondMoveKeepsOnTheNewWorkDrive(t *testing.T) {
	f := newMoveFixture(t)
	f.ms.site.WorkspaceProviders = keepSite(true, true, 3, 512).WorkspaceProviders
	f.srv.cfg.Store = &moveDriveStore{moveStore: f.ms, md: newMemberDriveStore()}
	f.srv.cfg.Runner = &moveDriveRunner{moveRunner: f.mr}
	f.srv.cfg.RunnerTarget = "k8s"
	f.st.mu.Lock()
	f.st.run.CreatedBy = aliceSub
	f.st.mu.Unlock()
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
	to, err := types.ParseMoveDestination("org")
	if err != nil {
		t.Fatal(err)
	}
	moveAsAlice := func() (types.RunMove, error) {
		mv, err := f.srv.startMove(personCtx(aliceSub), moveRequest{Run: f.current(t), To: to, ActorType: types.ActorHuman, Principal: aliceSub})
		f.srv.bg.Wait()
		return mv, err
	}
	first, err := moveAsAlice()
	if err != nil || f.only(t).State != types.MoveCommitted {
		t.Fatalf("first move: %v, row %+v; want committed", err, f.only(t))
	}
	workID := moveWorkDriveID(f.run.ID, first.ID)
	rec, err := f.srv.loadRunSandboxSpec(context.Background(), f.run.ID)
	if err != nil || len(rec.Drives) != 1 || rec.Drives[0].DriveID != workID || rec.Drives[0].Backend != types.DriveBackendK8sPVC ||
		rec.Drives[0].Target != runner.ScratchWorkPath {
		t.Fatalf("record after the move = %+v, %v; want the new work drive alone", rec.Drives, err)
	}
	mu.Lock()
	asked = nil
	mu.Unlock()
	_, _ = moveAsAlice() // a second move: its keep is asked before anything that may refuse it
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 || len(asked[0]) != 1 || asked[0][0].DriveID != workID || asked[0][0].Backend != types.DriveBackendK8sPVC {
		t.Errorf("the second move's source keep saw %+v, want the new work drive", asked)
	}
}

// moveDriveStore is the move fixture's store with a person's drives kept by md.
type moveDriveStore struct {
	*moveStore
	md *memberDriveStore
}

var _ store.MemberDriveStore = (*moveDriveStore)(nil)

func (s *moveDriveStore) CreateMemberDrive(ctx context.Context, d types.UserDrive, limit int) (types.UserDrive, error) {
	return s.md.CreateMemberDrive(ctx, d, limit)
}
func (s *moveDriveStore) ListMemberDrivesOf(ctx context.Context, owner string) ([]types.UserDrive, error) {
	return s.md.ListMemberDrivesOf(ctx, owner)
}
func (s *moveDriveStore) GetMemberDriveOf(ctx context.Context, owner string, id uuid.UUID) (types.UserDrive, error) {
	return s.md.GetMemberDriveOf(ctx, owner, id)
}
func (s *moveDriveStore) GetMemberDrive(ctx context.Context, id uuid.UUID) (types.UserDrive, error) {
	return s.md.GetMemberDrive(ctx, id)
}
func (s *moveDriveStore) BeginMemberDriveDelete(ctx context.Context, owner string, id uuid.UUID) (types.UserDrive, time.Time, error) {
	return s.md.BeginMemberDriveDelete(ctx, owner, id)
}
func (s *moveDriveStore) BeginMemberDriveDeleteAdmin(ctx context.Context, id uuid.UUID) (types.UserDrive, time.Time, error) {
	return s.md.BeginMemberDriveDeleteAdmin(ctx, id)
}
func (s *moveDriveStore) MemberDriveOwnerStarting(ctx context.Context, owner string, fresh time.Duration) (bool, error) {
	return s.md.MemberDriveOwnerStarting(ctx, owner, fresh)
}
func (s *moveDriveStore) BeginMemberDriveTakeover(ctx context.Context, id uuid.UUID) (types.UserDrive, time.Time, error) {
	return s.md.BeginMemberDriveTakeover(ctx, id)
}
func (s *moveDriveStore) IssueMemberDriveDelete(ctx context.Context, id uuid.UUID, started time.Time, n int) (int, error) {
	return s.md.IssueMemberDriveDelete(ctx, id, started, n)
}
func (s *moveDriveStore) ClearMemberDriveClaim(ctx context.Context, id uuid.UUID, started time.Time, n int) error {
	return s.md.ClearMemberDriveClaim(ctx, id, started, n)
}
func (s *moveDriveStore) ClearMemberDriveIssued(ctx context.Context, id uuid.UUID, started time.Time, claimN, issued int) error {
	return s.md.ClearMemberDriveIssued(ctx, id, started, claimN, issued)
}
func (s *moveDriveStore) FinishMemberDriveDelete(ctx context.Context, id uuid.UUID, started time.Time, n int) error {
	return s.md.FinishMemberDriveDelete(ctx, id, started, n)
}
func (s *moveDriveStore) ListMemberDrives(ctx context.Context, p store.Page) ([]types.UserDrive, int, error) {
	return s.md.ListMemberDrives(ctx, p)
}

// moveDriveRunner is the move fixture's runner on a deployment that mounts and deletes a person's drives.
type moveDriveRunner struct{ *moveRunner }

func (r *moveDriveRunner) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	c, err := r.moveRunner.Capabilities(ctx)
	c.UserDrives = true
	return c, err
}
func (*moveDriveRunner) CanManageDrives(context.Context) error { return nil }
func (*moveDriveRunner) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	return runner.DriveReclaimDeleted, nil
}
