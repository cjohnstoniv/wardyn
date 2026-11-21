// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keepEnv is the queue env with member drives and keep_work_folder on at limit drives per person, and a pool of one slot
// that an occupant holds, so a keep launch waits.
func keepEnv(t *testing.T, limit int) (*queueEnv, types.RunnerPool) {
	t.Helper()
	e := newQueueEnv(t)
	ctx := context.Background()
	e.srv.cfg.RunnerTarget = "docker"
	e.srv.cfg.Runner = &keepPoolRunner{e.runner}
	site, err := e.pg.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	site.WorkspaceProviders = keepSite(true, true, limit, 0).WorkspaceProviders
	if _, err := e.pg.PutSiteConfig(ctx, site); err != nil {
		t.Fatal(err)
	}
	pool := e.cappedPool("Shared", 1)
	e.occupy(pool)
	return e, pool
}

func (e *queueEnv) keptDriveExists(run types.AgentRun) bool {
	e.t.Helper()
	_, err := e.pg.GetMemberDriveOf(context.Background(), types.CanonicalUserSubject(run.CreatedBy), keptWorkFolderDriveID(run.ID))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		e.t.Fatal(err)
	}
	return err == nil
}

// #3399: a keep_work_folder run that ends without ever starting deletes its kept drive, so the person's
// max_per_person count is free again. At a limit of one, the person can start another kept-drive run afterwards.
func TestPG_KeepRunThatNeverStartsFreesItsDrive(t *testing.T) {
	cases := []struct {
		name string
		end  func(t *testing.T, e *queueEnv, run types.AgentRun)
	}{
		{"cancelled while queued", func(t *testing.T, e *queueEnv, run types.AgentRun) {
			if w := e.admin(http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", ""); w.Code != http.StatusAccepted {
				t.Fatalf("kill = %d %s, want 202", w.Code, w.Body)
			}
		}},
		{"queue deadline passes", func(t *testing.T, e *queueEnv, run types.AgentRun) {
			e.exec(`UPDATE run_queue SET deadline_at = now() - interval '1 minute' WHERE run_id = $1`, run.ID)
			e.sweep()
			if got := e.run(run.ID); got.State != types.RunFailed {
				t.Fatalf("run is %s, want FAILED by the queue deadline", got.State)
			}
		}},
		{"the component fence aborts it after it committed", func(t *testing.T, e *queueEnv, run types.AgentRun) {
			e.srv.abortQueuedRun(context.Background(), run.ID, createRunComponentsAbortHint)
			if got := e.run(run.ID); got.State != types.RunFailed {
				t.Fatalf("run is %s, want FAILED by the abort", got.State)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, pool := keepEnv(t, 1)
			waiting := e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`)
			run := e.run(waiting.ID)
			if !e.keptDriveExists(run) {
				t.Fatal("the queued keep run has no kept drive to begin with")
			}
			// At the limit of one, the drive refuses a second keep run for as long as it stays.
			if _, w := e.launch(e.person, e.reread(pool), `,"keep_work_folder":true`); w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("a second keep run at the limit = %d %s, want 422", w.Code, w.Body)
			}
			tc.end(t, e, run)
			if e.keptDriveExists(run) {
				t.Errorf("the kept drive of a run that never started is still there")
			}
			if _, w := e.launch(e.person, e.reread(pool), `,"keep_work_folder":true`); w.Code != http.StatusCreated {
				t.Errorf("a second keep run after the first never started = %d %s, want 201", w.Code, w.Body)
			}
		})
	}
}

// reclaimInUse is the keep env's runner whose substrate holds every drive: another run has it mounted.
type reclaimInUse struct{ *keepPoolRunner }

func (reclaimInUse) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	return "", runner.ErrDriveInUse
}

// The kept drive is an ordinary member drive, so another run of the person can mount it. A queued run that
// ends while one holds it leaves the drive where it is, and the audit row says the delete was refused, never
// that the object was already absent: the two in-use guards of a person's own delete run for it too.
func TestPG_KeepRunThatNeverStartsLeavesTheDriveAnotherRunHolds(t *testing.T) {
	cases := []struct {
		name string
		hold func(e *queueEnv, owner string)
	}{
		{"another run of the owner is starting", func(e *queueEnv, owner string) {
			e.exec(`UPDATE agent_runs SET state = 'STARTING', created_by = $1 WHERE created_by = 'occupant'`, owner)
		}},
		{"the substrate has the drive mounted", func(e *queueEnv, _ string) {
			e.srv.cfg.Runner = reclaimInUse{&keepPoolRunner{e.runner}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, pool := keepEnv(t, 1)
			run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
			tc.hold(e, run.CreatedBy)
			if w := e.admin(http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", ""); w.Code != http.StatusAccepted {
				t.Fatalf("kill = %d %s, want 202", w.Code, w.Body)
			}
			if !e.keptDriveExists(run) {
				t.Errorf("the kept drive was deleted while another run holds it")
			}
			// A refusal leaves no tombstone behind: a drive marked for delete is gone from every run's reach.
			var tombstoned bool
			if err := e.poolEnv.pool.QueryRow(context.Background(),
				`SELECT delete_started_at IS NOT NULL FROM user_drives WHERE id = $1`, keptWorkFolderDriveID(run.ID)).Scan(&tombstoned); err != nil {
				t.Fatal(err)
			}
			if tombstoned {
				t.Errorf("the refused drop left delete_started_at set on the kept drive")
			}
			var rows []map[string]any
			for _, ev := range e.h.audit.snapshot() {
				if ev.Action != "drive.member.delete" {
					continue
				}
				var data map[string]any
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, data)
			}
			if len(rows) != 1 || rows[0]["outcome"] != "refused" || rows[0]["by"] != memberDriveByRunEnd {
				t.Errorf("drive.member.delete rows = %v, want one refused by %s, never already_absent", rows, memberDriveByRunEnd)
			}
		})
	}
}

// A run that DID start keeps its drive, however it ends: only a run that never started gives it up.
func TestPG_KeepRunThatStartedKeepsItsDrive(t *testing.T) {
	e := newQueueEnv(t)
	ctx := context.Background()
	e.srv.cfg.RunnerTarget = "docker"
	e.srv.cfg.Runner = &keepPoolRunner{e.runner}
	site, err := e.pg.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	site.WorkspaceProviders = keepSite(true, true, 1, 0).WorkspaceProviders
	if _, err := e.pg.PutSiteConfig(ctx, site); err != nil {
		t.Fatal(err)
	}
	pool := e.cappedPool("Shared", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`)
	e.free(occupant)
	e.sweep()
	started := e.waitStarted(waiting.ID)
	if w := e.admin(http.MethodPost, "/api/v1/runs/"+started.ID.String()+"/kill", ""); w.Code != http.StatusAccepted {
		t.Fatalf("kill of the started run = %d %s, want 202", w.Code, w.Body)
	}
	if !e.keptDriveExists(started) {
		t.Errorf("a run that started lost its kept drive when it ended")
	}
}

// A kept work folder belongs to the run it was kept from until that run has ended: another run of the person
// asking for it is refused as for a drive that is not there (drive_not_found), and once the run has ended it is
// offered. Otherwise a run could write to it and end, and then the drop of a queued run that never started would
// delete that work.
func TestPG_KeptDriveIsOfferedOnlyOnceItsRunHasEnded(t *testing.T) {
	e := newQueueEnv(t)
	ctx := context.Background()
	e.srv.cfg.RunnerTarget = "docker"
	e.srv.cfg.Runner = &keepPoolRunner{e.runner}
	site, err := e.pg.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	site.WorkspaceProviders = keepSite(true, true, 3, 0).WorkspaceProviders
	if _, err := e.pg.PutSiteConfig(ctx, site); err != nil {
		t.Fatal(err)
	}
	pool := e.cappedPool("Shared", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`)
	ask := func() *httptest.ResponseRecorder {
		body := strings.Replace(endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`),
			`"no_repositories_or_drives":true`, fmt.Sprintf(`"drives":[{"drive_id":%q}]`, keptWorkFolderDriveID(waiting.ID)), 1)
		return e.call(e.person, http.MethodPost, "/api/v1/runs", body)
	}
	refused := func(when string) {
		t.Helper()
		if w := ask(); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), string(authz.ReasonDriveNotFound)) {
			t.Fatalf("asking for the kept drive %s = %d %s, want 422 drive_not_found", when, w.Code, w.Body)
		}
	}
	refused("while its run is queued")
	e.free(occupant)
	e.sweep()
	started := e.waitStarted(waiting.ID)
	refused("while its run is running")
	if w := e.admin(http.MethodPost, "/api/v1/runs/"+started.ID.String()+"/kill", ""); w.Code != http.StatusAccepted {
		t.Fatalf("kill = %d %s, want 202", w.Code, w.Body)
	}
	e.eventually("the kept run to end", func() bool { return e.run(started.ID).State.IsTerminal() })
	if w := ask(); w.Code != http.StatusCreated {
		t.Errorf("asking for the kept drive after its run ended = %d %s, want 201", w.Code, w.Body)
	}
}

// reclaimCounts is the keep env's runner counting the substrate reclaims it is asked for.
type reclaimCounts struct {
	*keepPoolRunner
	n *atomic.Int32
}

func (r reclaimCounts) ReclaimDrive(ctx context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	r.n.Add(1)
	return r.keepPoolRunner.ReclaimDrive(ctx, m)
}

// askForKeptDrive is a later run of the person asking to attach run's kept drive: the answer.
func (e *queueEnv) askForKeptDrive(pool types.RunnerPool, run types.AgentRun) *httptest.ResponseRecorder {
	body := strings.Replace(endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`),
		`"no_repositories_or_drives":true`, fmt.Sprintf(`"drives":[{"drive_id":%q}]`, keptWorkFolderDriveID(run.ID)), 1)
	return e.call(e.person, http.MethodPost, "/api/v1/runs", body)
}

func (e *queueEnv) keptDriveTombstoned(run types.AgentRun) bool {
	e.t.Helper()
	var held bool
	if err := e.poolEnv.pool.QueryRow(context.Background(),
		`SELECT delete_started_at IS NOT NULL FROM user_drives WHERE id = $1`, keptWorkFolderDriveID(run.ID)).Scan(&held); err != nil {
		e.t.Fatal(err)
	}
	return held
}

// A pool removal ends every waiting run in one statement and cleans them up one at a time afterwards, so a cleanup
// can run well after its run ended, and a kept drive is offered to its owner's other runs once its run has ended.
// The drive therefore carries the tombstone of its delete from the statement that ends the run: a later run that
// asks for it before the cleanup is refused as for a drive that is not there, and the cleanup then reclaims it.
// Nothing is decided by comparing times: the stored creation time of a queued run is the application's clock
// translated back, so it is no evidence of which of two events came first.
func TestPG_KeptDriveIsHeldFromTheEndOfItsRunUntilItsCleanup(t *testing.T) {
	e, pool := keepEnv(t, 3)
	var calls atomic.Int32
	e.srv.cfg.Runner = reclaimCounts{&keepPoolRunner{e.runner}, &calls}
	ctx := context.Background()
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
	if e.keptDriveTombstoned(run) {
		t.Fatal("the drive of a run still waiting carries a tombstone")
	}
	// The run ends by the statement alone: the cleanup that follows it is the one that runs late.
	if ended, err := e.pg.ExitQueuedRun(ctx, run.ID, types.RunFailed, "queue: runner_pool_removed", "pool removed"); err != nil || !ended {
		t.Fatalf("ExitQueuedRun = %v %v, want the run ended", ended, err)
	}
	if !e.keptDriveTombstoned(run) {
		t.Fatal("the kept drive carries no tombstone the moment its run ended, so a later run could be offered it before the cleanup")
	}
	if w := e.askForKeptDrive(pool, run); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), string(authz.ReasonDriveNotFound)) {
		t.Fatalf("a later run asking for the kept drive before its cleanup = %d %s, want 422 drive_not_found", w.Code, w.Body)
	}
	e.srv.finishQueuedKeptDrive(ctx, run.ID) // the delayed first cleanup
	if e.keptDriveExists(run) || calls.Load() != 1 {
		t.Errorf("after the cleanup the kept drive exists = %v with %d reclaims, want it gone with one", e.keptDriveExists(run), calls.Load())
	}
}

// A later run asks for each kept drive while its delayed cleanup runs: whichever finishes first, a run that was
// admitted with a drive never loses it.
func TestPG_KeptDriveCleanupConcurrentWithALaterRunsAdmission(t *testing.T) {
	const drives = 6
	e, pool := keepEnv(t, drives)
	ctx := context.Background()
	var runs []types.AgentRun
	for i := 0; i < drives; i++ {
		run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
		if ended, err := e.pg.ExitQueuedRun(ctx, run.ID, types.RunFailed, "queue: runner_pool_removed", "pool removed"); err != nil || !ended {
			t.Fatalf("ExitQueuedRun = %v %v, want the run ended", ended, err)
		}
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
		if codes[i] == http.StatusCreated && !e.keptDriveExists(run) {
			t.Errorf("drive %d: a later run was admitted with it and the cleanup deleted it", i)
		}
		if codes[i] != http.StatusCreated && codes[i] != http.StatusUnprocessableEntity {
			t.Errorf("drive %d: the later run's answer is %d, want 201 or 422", i, codes[i])
		}
	}
}

// A drive the cleanup keeps, because the substrate has it mounted or its owner has a run starting, has its tombstone
// cleared and is offered from then on: a later run attaches it, starts and gets it mounted. The drive stays.
func TestPG_KeptDriveTheCleanupKeepsIsAttachableByALaterRun(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	e.srv.cfg.Runner = reclaimInUse{&keepPoolRunner{e.runner}}
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
	if ended, err := e.pg.ExitQueuedRun(ctx, run.ID, types.RunFailed, "queue: runner_pool_removed", "pool removed"); err != nil || !ended {
		t.Fatalf("ExitQueuedRun = %v %v, want the run ended", ended, err)
	}
	e.srv.finishQueuedKeptDrive(ctx, run.ID)
	if !e.keptDriveExists(run) || e.keptDriveTombstoned(run) {
		t.Fatalf("the drive the substrate refused to reclaim exists = %v, tombstoned = %v; want it kept with no tombstone", e.keptDriveExists(run), e.keptDriveTombstoned(run))
	}
	w := e.askForKeptDrive(pool, run)
	if w.Code != http.StatusCreated {
		t.Fatalf("a later run asking for the kept drive after the cleanup kept it = %d %s, want 201", w.Code, w.Body)
	}
	var later types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &later); err != nil {
		t.Fatal(err)
	}
	var occupant uuid.UUID
	if err := e.poolEnv.pool.QueryRow(ctx, `SELECT id FROM agent_runs WHERE created_by = 'occupant'`).Scan(&occupant); err != nil {
		t.Fatal(err)
	}
	e.free(e.run(occupant))
	e.sweep()
	e.waitStarted(later.ID)
	mounted := false
	for _, m := range e.specFor(later.ID).Drives {
		mounted = mounted || m.DriveID == keptWorkFolderDriveID(run.ID)
	}
	if !mounted || !e.keptDriveExists(run) {
		t.Errorf("the later run mounted the kept drive = %v, the drive exists = %v; want both", mounted, e.keptDriveExists(run))
	}
}

// A run that ends while its owner's own delete of the drive is pending leaves that tombstone alone: the exit has
// no claim of its own. If that delete then fails and clears its tombstone, a later run attaches the drive, mounts it
// and ends, and the run's delayed cleanup must keep the drive: it has no claim to finish, and the direct path's
// delete is not for a queued run.
func TestPG_KeptDriveOwnersFailedDeleteIsNotTakenOverByTheDelayedCleanup(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	var calls atomic.Int32
	e.srv.cfg.Runner = reclaimCounts{&keepPoolRunner{e.runner}, &calls}
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
	owner := types.CanonicalUserSubject(run.CreatedBy)
	_, started, err := e.pg.BeginMemberDriveDelete(ctx, owner, keptWorkFolderDriveID(run.ID))
	if err != nil {
		t.Fatalf("the owner's delete begins: %v", err)
	}
	if ended, err := e.pg.ExitQueuedRun(ctx, run.ID, types.RunFailed, "queue: runner_pool_removed", "pool removed"); err != nil || !ended {
		t.Fatalf("ExitQueuedRun = %v %v, want the run ended", ended, err)
	}
	var tombstone time.Time
	if err := e.poolEnv.pool.QueryRow(ctx, `SELECT delete_started_at FROM user_drives WHERE id = $1`, keptWorkFolderDriveID(run.ID)).Scan(&tombstone); err != nil || !tombstone.Equal(started) {
		t.Fatalf("tombstone after the exit = %v (%v), want the owner's %v untouched", tombstone, err, started)
	}
	// The owner's delete fails on the substrate and clears its own tombstone.
	if err := e.pg.ClearMemberDriveClaim(ctx, keptWorkFolderDriveID(run.ID), started, 0); err != nil {
		t.Fatal(err)
	}
	w := e.askForKeptDrive(pool, run)
	if w.Code != http.StatusCreated {
		t.Fatalf("a later run asking for the drive after the owner's delete failed = %d %s, want 201", w.Code, w.Body)
	}
	var later types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &later); err != nil {
		t.Fatal(err)
	}
	var occupant uuid.UUID
	if err := e.poolEnv.pool.QueryRow(ctx, `SELECT id FROM agent_runs WHERE created_by = 'occupant'`).Scan(&occupant); err != nil {
		t.Fatal(err)
	}
	e.free(e.run(occupant))
	e.sweep()
	e.waitStarted(later.ID)
	mounted := false
	for _, m := range e.specFor(later.ID).Drives {
		mounted = mounted || m.DriveID == keptWorkFolderDriveID(run.ID)
	}
	if !mounted {
		t.Fatal("the later run did not mount the drive")
	}
	if kw := e.admin(http.MethodPost, "/api/v1/runs/"+later.ID.String()+"/kill", ""); kw.Code != http.StatusAccepted {
		t.Fatalf("kill = %d %s, want 202", kw.Code, kw.Body)
	}
	e.eventually("the later run to end", func() bool { return e.run(later.ID).State.IsTerminal() })
	e.srv.finishQueuedKeptDrive(ctx, run.ID) // the delayed cleanup
	if !e.keptDriveExists(run) || calls.Load() != 0 || e.keptDriveTombstoned(run) {
		t.Errorf("after the delayed cleanup the drive exists = %v, reclaims = %d, tombstoned = %v; want it kept, untouched and unmarked",
			e.keptDriveExists(run), calls.Load(), e.keptDriveTombstoned(run))
	}
}

// raceOnDriveRead lands the queue exit of a kept run right after the drive read admission makes, which is the
// instant a read of the drive and a read of its source run, made apart, would straddle.
type raceOnDriveRead struct {
	store.PG
	after func()
}

func (s *raceOnDriveRead) ListMemberDrivesOf(ctx context.Context, owner string) ([]types.UserDrive, error) {
	out, err := s.PG.ListMemberDrivesOf(ctx, owner)
	s.land()
	return out, err
}

func (s *raceOnDriveRead) ListMemberDrivesOfWithHeld(ctx context.Context, owner string) ([]types.UserDrive, map[uuid.UUID]bool, error) {
	out, held, err := s.PG.ListMemberDrivesOfWithHeld(ctx, owner)
	s.land()
	return out, held, err
}

func (s *raceOnDriveRead) land() {
	if hook := s.after; hook != nil {
		s.after = nil
		hook()
	}
}

// An admission whose drive read comes just before the source run's queue exit is refused the drive as one still
// held by that run: the drive and the run's state are one snapshot, so it cannot be offered a drive the same exit
// has just marked, and then be unable to mount it.
func TestPG_KeptDriveAdmissionSpanningItsRunsExit(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
	race := &raceOnDriveRead{PG: e.pg}
	e.srv.cfg.Store = race
	race.after = func() {
		if ended, err := e.pg.ExitQueuedRun(ctx, run.ID, types.RunFailed, "queue: runner_pool_removed", "pool removed"); err != nil || !ended {
			t.Errorf("ExitQueuedRun = %v %v, want the run ended", ended, err)
		}
	}
	if w := e.askForKeptDrive(pool, run); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), string(authz.ReasonDriveNotFound)) {
		t.Errorf("an admission whose drive read preceded the run's end = %d %s, want 422 drive_not_found, not a run that cannot mount the drive", w.Code, w.Body)
	}
}
