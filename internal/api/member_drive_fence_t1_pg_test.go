// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The delete fence on a queued kept work folder (migration 0299), against a real Postgres: a worker that stalled
// after it read its claim makes no call once the claim moved, a crash on either side of the issue leaves the row a
// later holder can settle, a stuck claim is hidden and an administrator finishes it, and the claim is taken by the
// statement that ends the run. Guarded by WARDYN_TEST_PG through the queue env: skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const fenceKeepFolder = `,"keep_work_folder":true`

// fenceKeptRun is a queued keep_work_folder run of the keep env's person, and its drive's id.
func (e *queueEnv) fenceKeptRun(pool types.RunnerPool) (types.AgentRun, uuid.UUID) {
	e.t.Helper()
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), fenceKeepFolder).ID)
	return run, keptWorkFolderDriveID(run.ID)
}

// fenceEndKept ends run in the queue by the statement alone, as a kill, a timeout or a pool removal does: the claim is
// taken in it, and no cleanup has run.
func (e *queueEnv) fenceEndKept(run types.AgentRun) {
	e.t.Helper()
	if ended, err := e.pg.ExitQueuedRun(context.Background(), run.ID, types.RunFailed, "queue: runner_pool_removed", "pool removed"); err != nil || !ended {
		e.t.Fatalf("ExitQueuedRun = %v %v, want the run ended", ended, err)
	}
}

// fenceMarkOf reads the drive's mark and its issue count.
func (e *queueEnv) fenceMarkOf(id uuid.UUID) (marked bool, issues int) {
	e.t.Helper()
	if err := e.poolEnv.pool.QueryRow(context.Background(),
		`SELECT delete_started_at IS NOT NULL, delete_issues FROM user_drives WHERE id = $1`, id).Scan(&marked, &issues); err != nil {
		e.t.Fatal(err)
	}
	return marked, issues
}

// fenceAgedClaim sets the drive's mark, and the run's end it was taken at, to the same instant 61 seconds ago: a claim
// the takeover of an administrator or of the sweep may replace, still equal to its run's end.
func (e *queueEnv) fenceAgedClaim(run types.AgentRun, drive uuid.UUID) time.Time {
	e.t.Helper()
	at := time.Now().Add(-61 * time.Second).Truncate(time.Microsecond)
	e.exec(`UPDATE agent_runs SET ended_at = $2 WHERE id = $1`, run.ID, at)
	e.exec(`UPDATE user_drives SET delete_started_at = $2 WHERE id = $1`, drive, at)
	return at
}

// fencePausedWorker is the store of a worker that stops after it holds its claim and before its issue: the first caller
// of MemberDriveOwnerStarting (deleteMemberDrive's last read before the issue) waits for resume. While starting is
// set every other caller is told the owner has a run starting.
type fencePausedWorker struct {
	store.PG
	paused, resume chan struct{}
	calls          atomic.Int32
	starting       atomic.Bool
}

func newFencePausedWorker(pg store.PG) *fencePausedWorker {
	return &fencePausedWorker{PG: pg, paused: make(chan struct{}), resume: make(chan struct{})}
}

func (s *fencePausedWorker) MemberDriveOwnerStarting(ctx context.Context, owner string, fresh time.Duration) (bool, error) {
	if s.calls.Add(1) == 1 {
		close(s.paused)
		<-s.resume
	} else if s.starting.Load() {
		return true, nil
	}
	return s.PG.MemberDriveOwnerStarting(ctx, owner, fresh)
}

// A worker that read its claim and stalled before its issue, while another holder settled the drive and a later run
// was admitted, makes no call when it resumes: its issue finds the pair moved. Rows 1 and 6 of the interleavings.
func TestPG_DriveFence_ResumedWorkerMakesNoCall(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*queueEnv, types.RunnerPool, *atomic.Int32, *fencePausedWorker) {
		e, pool := keepEnv(t, 3)
		var calls atomic.Int32
		e.srv.cfg.Runner = reclaimCounts{&keepPoolRunner{e.runner}, &calls}
		w := newFencePausedWorker(e.pg)
		e.srv.cfg.Store = w
		return e, pool, &calls, w
	}
	// afterwards: the drive is there, unmarked, at the count the other holder left, and the resumed worker called nothing.
	afterwards := func(t *testing.T, e *queueEnv, drive uuid.UUID, calls *atomic.Int32) {
		t.Helper()
		if marked, n := e.fenceMarkOf(drive); marked || n != 2 {
			t.Errorf("after the resumed worker: marked = %v, count = %d; want unmarked at 2 (the other holder's clear)", marked, n)
		}
		if calls.Load() != 0 {
			t.Errorf("substrate calls = %d, want 0: the resumed worker's issue found its pair moved", calls.Load())
		}
	}

	t.Run("a queue cleanup resumed after another holder took over and cleared (row 1)", func(t *testing.T) {
		e, pool, calls, w := setup(t)
		run, drive := e.fenceKeptRun(pool)
		e.fenceEndKept(run)
		e.fenceAgedClaim(run, drive) // the claim Q reads is a minute old, equal to its run's end
		done := make(chan struct{})
		go func() { defer close(done); e.srv.finishQueuedKeptDrive(ctx, run.ID) }()
		<-w.paused
		// K takes the claim over, is answered definitively, and clears; L is then admitted with the drive.
		d, sk, err := e.pg.BeginMemberDriveDeleteAdmin(ctx, drive)
		if err != nil {
			t.Fatal(err)
		}
		issued, err := e.pg.IssueMemberDriveDelete(ctx, drive, sk, d.DeleteIssues)
		if err != nil || issued != 1 {
			t.Fatalf("K's issue = %d, %v; want 1", issued, err)
		}
		if err := e.pg.ClearMemberDriveIssued(ctx, drive, sk, d.DeleteIssues, issued); err != nil {
			t.Fatal(err)
		}
		if got := e.askForKeptDrive(pool, run); got.Code != http.StatusCreated {
			t.Fatalf("a later run asking for the cleared drive = %d %s, want 201", got.Code, got.Body)
		}
		close(w.resume)
		<-done
		afterwards(t, e, drive, calls)
	})

	t.Run("an owner's delete resumed after another holder cleared at its pre-flight (row 6)", func(t *testing.T) {
		e, pool, calls, w := setup(t)
		run, drive := e.fenceKeptRun(pool)
		owner := types.CanonicalUserSubject(run.CreatedBy)
		d, _, err := e.pg.BeginMemberDriveDelete(ctx, owner, drive) // O's claim, then aged to a minute old
		if err != nil {
			t.Fatal(err)
		}
		started := e.fenceAgedClaim(run, drive)
		e.fenceEndKept(run) // the run's end finds the owner's claim and takes none
		done := make(chan struct{})
		go func() { defer close(done); e.srv.deleteMemberDrive(ctx, w, d, started, memberDriveByOwner) }()
		<-w.paused
		// K takes the claim over and fails before its call: the owner has a run starting, so it clears at its pre-flight.
		dk, sk, err := e.pg.BeginMemberDriveDeleteAdmin(ctx, drive)
		if err != nil {
			t.Fatal(err)
		}
		w.starting.Store(true)
		e.srv.deleteMemberDrive(ctx, w, dk, sk, memberDriveByAdmin)
		w.starting.Store(false)
		if got := e.askForKeptDrive(pool, run); got.Code != http.StatusCreated {
			t.Fatalf("a later run asking for the cleared drive = %d %s, want 201", got.Code, got.Body)
		}
		close(w.resume)
		<-done
		afterwards(t, e, drive, calls)
	})
}

// fenceCrashAtIssue is the store of a worker whose process stops at its issue: before the statement runs, or after it
// committed and before the call. The goroutine is ended there, so nothing after the issue (the call, the settle, the
// audit) runs, as when the process dies.
type fenceCrashAtIssue struct {
	store.PG
	afterCommit bool
}

func (s fenceCrashAtIssue) IssueMemberDriveDelete(ctx context.Context, id uuid.UUID, started time.Time, n int) (int, error) {
	if s.afterCommit {
		if _, err := s.PG.IssueMemberDriveDelete(ctx, id, started, n); err != nil {
			return 0, err
		}
	}
	runtime.Goexit()
	return 0, nil
}

// A process that stops before its issue commits leaves a claim nothing was issued under, which a later holder may
// clear on a definitive answer; one that stops after it leaves an odd count, which a later holder finishes and can
// never clear. Neither made a call. Row 10.
func TestPG_DriveFence_CrashOnEitherSideOfTheIssue(t *testing.T) {
	ctx := context.Background()
	stopped := func(t *testing.T, afterCommit bool) (*queueEnv, *atomic.Int32, types.AgentRun, uuid.UUID) {
		e, pool := keepEnv(t, 3)
		var calls atomic.Int32
		e.srv.cfg.Runner = reclaimCounts{&keepPoolRunner{e.runner}, &calls}
		run, drive := e.fenceKeptRun(pool)
		e.fenceEndKept(run)
		e.srv.cfg.Store = fenceCrashAtIssue{PG: e.pg, afterCommit: afterCommit}
		done := make(chan struct{})
		go func() { defer close(done); e.srv.finishQueuedKeptDrive(ctx, run.ID) }()
		<-done
		e.srv.cfg.Store = e.pg
		if calls.Load() != 0 {
			t.Fatalf("substrate calls before the process stopped = %d, want 0", calls.Load())
		}
		e.exec(`UPDATE user_drives SET delete_started_at = now() - interval '61 seconds' WHERE id = $1`, drive)
		return e, &calls, run, drive
	}

	t.Run("before the issue commits", func(t *testing.T) {
		e, calls, run, drive := stopped(t, false)
		if marked, n := e.fenceMarkOf(drive); !marked || n != 0 {
			t.Fatalf("after the stop: marked = %v, count = %d; want a claim at 0", marked, n)
		}
		calls.Store(0)
		// a definitive answer: another run has the drive
		e.srv.cfg.Runner = fenceReclaimByDrive{&keepPoolRunner{e.runner}, map[uuid.UUID]bool{drive: true}, calls}
		d, started, err := e.pg.BeginMemberDriveDeleteAdmin(ctx, drive)
		if err != nil {
			t.Fatal(err)
		}
		e.srv.deleteMemberDrive(ctx, e.pg, d, started, memberDriveByAdmin)
		if marked, n := e.fenceMarkOf(drive); marked || n != 2 {
			t.Errorf("after the later holder's definitive answer: marked = %v, count = %d; want cleared at 2", marked, n)
		}
		if !e.keptDriveExists(run) || calls.Load() != 1 {
			t.Errorf("drive exists = %v, later holder's calls = %d; want the drive kept after one call", e.keptDriveExists(run), calls.Load())
		}
	})

	t.Run("after the issue commits", func(t *testing.T) {
		e, calls, run, drive := stopped(t, true)
		if marked, n := e.fenceMarkOf(drive); !marked || n != 1 {
			t.Fatalf("after the stop: marked = %v, count = %d; want a claim at 1", marked, n)
		}
		if got, err := e.pg.GetMemberDrive(ctx, drive); err != nil || !got.Deleting() {
			t.Fatalf("the drive reads deleting = %v (%v), want true", got.Deleting(), err)
		}
		d, started, err := e.pg.BeginMemberDriveDeleteAdmin(ctx, drive)
		if err != nil || d.DeleteIssues != 1 {
			t.Fatalf("takeover = count %d, %v; want the odd count kept", d.DeleteIssues, err)
		}
		e.srv.deleteMemberDrive(ctx, e.pg, d, started, memberDriveByAdmin)
		if e.keptDriveExists(run) || calls.Load() != 1 {
			t.Errorf("drive exists = %v, calls = %d; want the later holder to finish it with one call", e.keptDriveExists(run), calls.Load())
		}
	})

	t.Run("after the issue commits, a later in-use answer cannot clear the odd count", func(t *testing.T) {
		e, _, run, drive := stopped(t, true)
		e.srv.cfg.Runner = reclaimInUse{&keepPoolRunner{e.runner}}
		d, started, err := e.pg.BeginMemberDriveDeleteAdmin(ctx, drive)
		if err != nil {
			t.Fatal(err)
		}
		e.srv.deleteMemberDrive(ctx, e.pg, d, started, memberDriveByAdmin)
		if marked, n := e.fenceMarkOf(drive); !marked || n != 3 || !e.keptDriveExists(run) {
			t.Errorf("after the in-use answer over an odd count: marked = %v, count = %d, exists = %v; want still deleting at 3", marked, n, e.keptDriveExists(run))
		}
	})
}

// A claim whose cleanup never ran (the process stopped between the end and its cleanup) stays hidden from its owner
// and counts against the limit; the administrator's list shows it Deleting, and the administrator's delete is
// refused until the mark is a minute old and then finishes it. Row 12, today's behaviour.
func TestPG_DriveFence_StuckClaimIsHiddenAndAnAdminFinishesIt(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	var calls atomic.Int32
	e.srv.cfg.Runner = reclaimCounts{&keepPoolRunner{e.runner}, &calls}
	run, drive := e.fenceKeptRun(pool)
	owner := types.CanonicalUserSubject(run.CreatedBy)
	control, err := e.pg.CreateMemberDrive(ctx, types.UserDrive{Name: "Control", Backend: types.DriveBackendDockerVolume, OwnerSubject: owner, CreatedBy: owner}, 3)
	if err != nil {
		t.Fatal(err)
	}
	e.fenceEndKept(run) // the claim is taken; the cleanup that would follow never runs

	w := e.call(e.person, http.MethodGet, "/api/v1/me/drives", "")
	var cat client.MeDriveCatalogue
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cat) != nil {
		t.Fatalf("GET /me/drives = %d %s", w.Code, w.Body)
	}
	listed := map[string]bool{}
	for _, d := range cat.Drives {
		listed[d.ID] = true
	}
	if !listed[control.ID.String()] || listed[drive.String()] {
		t.Errorf("the owner's catalogue lists the control drive = %v and the stuck claim = %v; want true and false", listed[control.ID.String()], listed[drive.String()])
	}
	if cat.Create == nil || cat.Create.Count != 2 {
		t.Errorf("the owner's drive count = %+v, want 2 (the stuck claim counts against the limit)", cat.Create)
	}

	w = e.admin(http.MethodGet, "/api/v1/drives/member", "")
	var list client.MemberDriveList
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatalf("GET /drives/member = %d %s", w.Code, w.Body)
	}
	deleting := map[uuid.UUID]bool{}
	for _, d := range list.Drives {
		deleting[d.ID] = d.Deleting
	}
	if got, ok := deleting[drive]; !ok || !got {
		t.Errorf("the administrator's list shows the stuck claim present = %v, Deleting = %v; want both true", ok, got)
	}
	if got, ok := deleting[control.ID]; !ok || got {
		t.Errorf("the administrator's list shows the control drive present = %v, Deleting = %v; want true and false", ok, got)
	}

	if w := e.admin(http.MethodDelete, "/api/v1/drives/member/"+drive.String(), ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonMemberDriveBusy) {
		t.Fatalf("the administrator's delete of a fresh claim = %d %s, want 409 %s", w.Code, w.Body, reasonMemberDriveBusy)
	}
	if !e.keptDriveExists(run) || calls.Load() != 0 {
		t.Fatalf("after the refused delete: exists = %v, calls = %d; want the drive kept and no call", e.keptDriveExists(run), calls.Load())
	}
	e.exec(`UPDATE user_drives SET delete_started_at = now() - interval '61 seconds' WHERE id = $1`, drive)
	if w := e.admin(http.MethodDelete, "/api/v1/drives/member/"+drive.String(), ""); w.Code != http.StatusOK {
		t.Fatalf("the administrator's delete of a claim a minute old = %d %s, want 200", w.Code, w.Body)
	}
	if e.keptDriveExists(run) || calls.Load() != 1 {
		t.Errorf("after the delete: exists = %v, calls = %d; want the drive gone by one call", e.keptDriveExists(run), calls.Load())
	}
}

// fenceReclaimByDrive is the keep env's runner whose substrate has some drives mounted by another run.
type fenceReclaimByDrive struct {
	*keepPoolRunner
	inUse map[uuid.UUID]bool
	calls *atomic.Int32
}

func (r fenceReclaimByDrive) ReclaimDrive(ctx context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	r.calls.Add(1)
	if r.inUse[m.DriveID] {
		return "", runner.ErrDriveInUse
	}
	return r.keepPoolRunner.ReclaimDrive(ctx, m)
}

// A later run asks for each kept drive while its delayed cleanup runs, and half the drives are mounted by another
// run. Because the claim comes with the run's end, a drive whose object the cleanup deletes is never offered (every
// ask is refused, the row goes), and a drive it keeps is offered only once it is unmarked: a 201 means present and
// unmarked, and none is left marked once every worker is done.
func TestPG_KeptDriveConcurrentCleanupAndAdmissionWithHalfTheDrivesInUse(t *testing.T) {
	const drives = 6
	e, pool := keepEnv(t, drives)
	ctx := context.Background()
	var calls atomic.Int32
	inUse := map[uuid.UUID]bool{}
	e.srv.cfg.Runner = fenceReclaimByDrive{&keepPoolRunner{e.runner}, inUse, &calls}
	var runs []types.AgentRun
	for i := range drives {
		run, drive := e.fenceKeptRun(pool)
		inUse[drive] = i%2 == 1
		e.fenceEndKept(run)
		runs = append(runs, run)
	}
	codes := make([]int, drives)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, run := range runs {
		wg.Add(2)
		go func() { defer wg.Done(); <-start; codes[i] = e.askForKeptDrive(pool, run).Code }()
		go func() { defer wg.Done(); <-start; e.srv.finishQueuedKeptDrive(ctx, run.ID) }()
	}
	close(start)
	wg.Wait()
	for i, run := range runs {
		drive := keptWorkFolderDriveID(run.ID)
		exists := e.keptDriveExists(run)
		switch {
		case inUse[drive]:
			if !exists {
				t.Errorf("drive %d is mounted by another run and was deleted", i)
			} else if marked, _ := e.fenceMarkOf(drive); marked {
				t.Errorf("drive %d is kept and still marked after every worker is done", i)
			}
			if codes[i] != http.StatusCreated && codes[i] != http.StatusUnprocessableEntity {
				t.Errorf("drive %d: the later run's answer is %d, want 201 or 422", i, codes[i])
			}
		default:
			if exists {
				t.Errorf("drive %d is free and was not deleted", i)
			}
			if codes[i] != http.StatusUnprocessableEntity {
				t.Errorf("drive %d: the later run's answer is %d, want 422: a drive being deleted is never offered", i, codes[i])
			}
		}
	}
	if calls.Load() != drives {
		t.Errorf("substrate calls = %d, want one per drive (%d)", calls.Load(), drives)
	}
}

// fenceReclaimProbe is the keep env's runner recording, at each reclaim it is asked for, whether the drive's mark equals
// the end of the run it was kept from, and the drive's issue count.
type fenceReclaimProbe struct {
	*keepPoolRunner
	read func(drive uuid.UUID) (claimIsRunEnd bool, issues int)
	mu   *sync.Mutex
	seen map[uuid.UUID]fenceReclaimSeen
}

// fenceReclaimSeen is what a fenceReclaimProbe read of one drive.
type fenceReclaimSeen struct {
	claimIsRunEnd bool
	issues        int
}

func (r fenceReclaimProbe) ReclaimDrive(ctx context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	equal, issues := r.read(m.DriveID)
	r.mu.Lock()
	r.seen[m.DriveID] = fenceReclaimSeen{equal, issues}
	r.mu.Unlock()
	return r.keepPoolRunner.ReclaimDrive(ctx, m)
}

// A runner pool's removal through the API ends every waiting keep run in the one statement, which also takes each
// drive's claim at the end it wrote: when the cleanup reaches the substrate the mark equals the run's ended_at and
// the issue is committed (odd), and afterwards the drives are gone.
func TestPG_DriveFence_PoolRemovalClaimsEachKeptDriveAtTheRunsEnd(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	probe := fenceReclaimProbe{keepPoolRunner: &keepPoolRunner{e.runner}, mu: &sync.Mutex{}, seen: map[uuid.UUID]fenceReclaimSeen{}}
	probe.read = func(drive uuid.UUID) (bool, int) {
		var equal bool
		var issues int
		if err := e.poolEnv.pool.QueryRow(ctx, `SELECT COALESCE(d.delete_started_at = r.ended_at, false), d.delete_issues
			FROM user_drives d JOIN agent_runs r ON r.id = d.kept_from_run WHERE d.id = $1`, drive).Scan(&equal, &issues); err != nil {
			t.Errorf("read the drive at its reclaim: %v", err)
		}
		return equal, issues
	}
	e.srv.cfg.Runner = probe
	first, _ := e.fenceKeptRun(pool)
	second, _ := e.fenceKeptRun(pool)
	if w := e.admin(http.MethodDelete, "/api/v1/runner-pools/"+pool.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete the pool = %d %s, want 204", w.Code, w.Body)
	}
	for _, run := range []types.AgentRun{first, second} {
		drive := keptWorkFolderDriveID(run.ID)
		if got := e.run(run.ID); got.State != types.RunKilled || got.EndedAt == nil {
			t.Fatalf("run %s after the pool's removal: state %s, ended %v; want KILLED and ended", run.ID, got.State, got.EndedAt)
		}
		seen, ok := probe.seen[drive]
		if !ok || seen != (fenceReclaimSeen{claimIsRunEnd: true, issues: 1}) {
			t.Errorf("drive of run %s at its reclaim: seen = %+v (%v); want the mark equal to the run's end and issue count 1", run.ID, seen, ok)
		}
		if e.keptDriveExists(run) {
			t.Errorf("drive of run %s still exists after the cleanup", run.ID)
		}
	}
}
