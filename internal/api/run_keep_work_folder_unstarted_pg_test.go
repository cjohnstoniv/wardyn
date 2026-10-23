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
	"testing"

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
