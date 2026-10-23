// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

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
