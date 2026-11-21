// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The reclaim a kept drive is owed when its run leaves the queue without starting survives a failed first
// attempt, and is never made against a drive another run holds. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reclaimFails is the keep env's runner whose substrate cannot reclaim anything yet.
type reclaimFails struct{ *keepPoolRunner }

func (reclaimFails) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	return "", errors.New("the substrate is unreachable")
}

// keptReclaimDue is when the sweep next retries the reclaim run's kept drive owes; the zero time when it owes none.
func (e *queueEnv) keptReclaimDue(run types.AgentRun) time.Time {
	e.t.Helper()
	var due *time.Time
	if err := e.poolEnv.pool.QueryRow(context.Background(),
		`SELECT reclaim_due_at FROM user_drives WHERE id = $1`, keptWorkFolderDriveID(run.ID)).Scan(&due); err != nil {
		e.t.Fatal(err)
	}
	if due == nil {
		return time.Time{}
	}
	return *due
}

// makeKeptReclaimDue brings the drive's retry forward, as waiting out the delay would.
func (e *queueEnv) makeKeptReclaimDue(run types.AgentRun) {
	e.t.Helper()
	e.exec(`UPDATE user_drives SET reclaim_due_at = now() - interval '1 second' WHERE id = $1`, keptWorkFolderDriveID(run.ID))
}

// A run that leaves the queue and whose process stops before it drops the drive still owes the reclaim: the
// debt was written with the transition, a sweep before it is due leaves the drive, and the one after deletes
// it and frees the person's count. Catches a reclaim that exists only in the process that ended the run.
func TestPG_KeepRunNeverStartedStillOwesItsDriveAfterTheProcessStops(t *testing.T) {
	ctx := context.Background()
	e, pool := keepEnv(t, 1)
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
	// The process stops right after the transition: the store's exit ran, the drop never did.
	if ok, err := e.pg.ExitQueuedRun(ctx, run.ID, types.RunFailed, statusDetailQueuePrefix+string(types.RunEndQueueTimeout), "the wait ran out"); err != nil || !ok {
		t.Fatalf("exit the queued run = %v, %v", ok, err)
	}
	if !e.keptDriveExists(run) || e.keptReclaimDue(run).IsZero() {
		t.Fatalf("after the exit the drive exists = %v and is owed a reclaim at %v; want it there and owed", e.keptDriveExists(run), e.keptReclaimDue(run))
	}
	if err := e.srv.sweepKeptDriveReclaims(ctx); err != nil || !e.keptDriveExists(run) {
		t.Fatalf("a sweep before the reclaim is due = %v, drive exists %v; want it left", err, e.keptDriveExists(run))
	}
	e.makeKeptReclaimDue(run)
	if err := e.srv.sweepKeptDriveReclaims(ctx); err != nil {
		t.Fatal(err)
	}
	if e.keptDriveExists(run) {
		t.Error("the drive of a run that never started is still there after its reclaim fell due")
	}
	if _, w := e.launch(e.person, e.reread(pool), `,"keep_work_folder":true`); w.Code != http.StatusCreated {
		t.Errorf("a second keep run once the drive was reclaimed = %d %s, want 201", w.Code, w.Body)
	}
}

// A first attempt the substrate fails keeps the drive and the debt, deferred, so it is not retried on the next
// sweep; once due and the substrate answers, the retry deletes it.
func TestPG_KeepRunNeverStartedRetriesAFailedReclaim(t *testing.T) {
	ctx := context.Background()
	e, pool := keepEnv(t, 1)
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
	e.srv.cfg.Runner = reclaimFails{&keepPoolRunner{e.runner}}
	if w := e.admin(http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", ""); w.Code != http.StatusAccepted {
		t.Fatalf("kill = %d %s, want 202", w.Code, w.Body)
	}
	if !e.keptDriveExists(run) {
		t.Fatal("the drive went although the substrate failed the reclaim")
	}
	due := e.keptReclaimDue(run)
	if !due.After(time.Now().Add(5 * time.Minute)) {
		t.Fatalf("the failed reclaim is due again at %v; want it deferred, not retried on the next sweep", due)
	}
	e.srv.cfg.Runner = &keepPoolRunner{e.runner}
	e.makeKeptReclaimDue(run)
	if err := e.srv.sweepKeptDriveReclaims(ctx); err != nil {
		t.Fatal(err)
	}
	if e.keptDriveExists(run) {
		t.Error("the retry did not delete the drive once the substrate answered")
	}
}

// A drive another run holds is never reclaimed: one the substrate has mounted stays and its debt is cleared, so
// no later sweep takes it once that run ends; and one whose owner made another run after this run ended, which
// could have attached it, stays too.
func TestPG_KeepRunNeverStartedNeverReclaimsADriveAnotherRunHolds(t *testing.T) {
	ctx := context.Background()
	t.Run("the substrate has it mounted", func(t *testing.T) {
		e, pool := keepEnv(t, 1)
		run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
		e.srv.cfg.Runner = reclaimInUse{&keepPoolRunner{e.runner}}
		if w := e.admin(http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", ""); w.Code != http.StatusAccepted {
			t.Fatalf("kill = %d %s, want 202", w.Code, w.Body)
		}
		if !e.keptDriveExists(run) || !e.keptReclaimDue(run).IsZero() {
			t.Fatalf("drive exists %v, owed at %v; want it kept and no longer owed", e.keptDriveExists(run), e.keptReclaimDue(run))
		}
		e.srv.cfg.Runner = &keepPoolRunner{e.runner} // the run that held it has ended: that is no reason to take it
		if err := e.srv.sweepKeptDriveReclaims(ctx); err != nil || !e.keptDriveExists(run) {
			t.Errorf("a sweep after the holder ended = %v, drive exists %v; want it left", err, e.keptDriveExists(run))
		}
	})
	t.Run("the owner made another run after it ended", func(t *testing.T) {
		e, pool := keepEnv(t, 2)
		run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
		if ok, err := e.pg.ExitQueuedRun(ctx, run.ID, types.RunFailed, statusDetailQueuePrefix+string(types.RunEndQueueTimeout), "the wait ran out"); err != nil || !ok {
			t.Fatalf("exit = %v, %v", ok, err)
		}
		e.queuedLaunch(e.person, e.reread(pool), "") // made after the first ended, so able to attach its drive
		e.makeKeptReclaimDue(run)
		if err := e.srv.sweepKeptDriveReclaims(ctx); err != nil {
			t.Fatal(err)
		}
		if !e.keptDriveExists(run) || !e.keptReclaimDue(run).IsZero() {
			t.Errorf("drive exists %v, owed at %v; want it kept and no longer owed", e.keptDriveExists(run), e.keptReclaimDue(run))
		}
	})
}
