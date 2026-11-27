// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lcLatch records that a local operator contained run on its runner, as a STATE page does.
func lcLatch(t *testing.T, st store.PG, run types.AgentRun, runnerID uuid.UUID) {
	t.Helper()
	ohExec(t, st, `INSERT INTO run_local_containment (run_id, runner_id, contained_at, scope, operator_uid) VALUES ($1, $2, now(), 'run', 501)`,
		run.ID, runnerID)
}

// The STATE page that carries the latch records it in its own transaction before the run's STATE judge runs,
// so a run held for recovery is judged not held on that page and its loss is asked for; the latch is audited once,
// however often a page repeats it, and the run gets no lease.
func TestRunnerLocalContain_AuditOnceAndHeld_PG(t *testing.T) {
	f := ohRunnerFixture(t)
	run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
	ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 90*time.Minute)
	if rc := ohCheck(t, f.st, run.ID); !rc.Held {
		t.Fatal("the run is not held before the latch; the test proves nothing")
	}
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	row := runnerwire.RunState{RunID: run.ID, Ref: run.SandboxRef, SandboxPresent: true,
		LocalContain: &runnerwire.LocalContain{At: at, Scope: "run", UID: 501}}
	for i := range 2 {
		if err := finalStatePage(t, f.srv, f.rn.ID, row); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
			t.Fatalf("page %d carrying the latch = %v, want reconciliation required", i, err)
		}
	}
	if needs, _ := ohObservation(t, f, run); !needs {
		t.Fatal("the judge did not ask for the latched run's loss on the page that carried the latch")
	}
	if rc := ohCheck(t, f.st, run.ID); rc.Held || !rc.Covered {
		t.Fatalf("after the latch: held %v covered %v; want covered and not held", rc.Held, rc.Covered)
	}
	if got, err := f.st.RunLocallyContained(f.ctx, run.ID); err != nil || !got {
		t.Fatalf("RunLocallyContained = %v, %v; want true", got, err)
	}
	rows := f.srv.cfg.Audit.(*syncAudit).eventsFor(run.ID, auditRunnerLocalContain)
	if len(rows) != 1 || rows[0].Actor != "runner:"+f.rn.ID.String() {
		t.Fatalf("runner.local_contain.record rows = %+v, want one, by the runner", rows)
	}
	if _, err := f.srv.leaseSendable(f.ctx, f.st, run); !errors.Is(err, errRunnerContained) {
		t.Fatalf("leaseSendable for the latched run = %v, want errRunnerContained", err)
	}
}

// A run its runner holds by a local containment is refused at recovery check 6 and gets no lease.
func TestRecover_LocalContainedRunnerRunRefused_PG(t *testing.T) {
	f := rdNewFixture(t)
	f.window(t, store.OutageKindRunnerLink, &f.rn.ID, time.Hour, nil)
	run := f.newRun(t, rdRunOpts{onRunner: true, pauses: true})
	lcLatch(t, f.st, run.run, f.rn.ID)
	if held, err := f.st.RunHeldContainedOnRunner(t.Context(), run.run.ID); err != nil || !held {
		t.Fatalf("RunHeldContainedOnRunner = %v, %v; want held", held, err)
	}
	if pending, err := f.st.RunHasPendingRunnerAction(t.Context(), run.run.ID); err != nil || pending {
		t.Fatalf("RunHasPendingRunnerAction = %v, %v; want none, so the door is reached", pending, err)
	}
	if _, err := f.srv.leaseSendable(t.Context(), f.st, run.run); !errors.Is(err, errRunnerContained) {
		t.Fatalf("leaseSendable = %v, want errRunnerContained", err)
	}
	before := f.lineage(t, run.run.ID)
	a := f.ask(t, run.token, f.rn.ID)
	f.wantRefused(t, run, before, a, rdRefusal{code: http.StatusForbidden, reason: reasonRunHeldContained})
}

// Both sweeps mark a lapsed, latched run lost on their pass; its held twin without the latch stays unmarked.
func TestLocalContain_SweepMarksRunLost_PG(t *testing.T) {
	for name, sweep := range map[string]func(*testing.T, *runnerLeaseFixture){
		"sweepLapsedRunTokens": ohLapseSweep,
		"OfflineFromRunner":    ohOfflineSweep,
	} {
		t.Run(name, func(t *testing.T) {
			f := ohRunnerFixture(t)
			run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
			twin := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
			ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 90*time.Minute)
			for _, r := range []types.AgentRun{run, twin} {
				if rc := ohCheck(t, f.st, r.ID); !rc.Held {
					t.Fatal("a run under its runner's open window is not held before the latch")
				}
			}
			lcLatch(t, f.st, run, f.rn.ID)
			sweep(t, f)
			ohRequireMarked(t, f.st, run, "the latched run")
			ohRequireUnmarked(t, f.st, f.srv.cfg.Audit.(*syncAudit), twin, "the held twin")
		})
	}
}

// A revive of a latched run is refused with its own reason and queues no recover_containment, even for a run
// its runner also holds by a stop.
func TestRevive_LocalContainedRunRefused_PG(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.heldRun(t)
	lcLatch(t, f.st, run, f.rn.ID)
	stored, err := f.st.GetRun(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := f.srv.reviveRunProxy(f.ctx, stored, types.ActorSystem, "test", false)
	if rerr == nil || rerr.reason != reasonReviveLocalContained || rerr.status != http.StatusConflict {
		t.Fatalf("revive of the latched run = %+v, want 409 %s", rerr, reasonReviveLocalContained)
	}
	var n int
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'recover_containment'`, run.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("recover_containment rows = %d, %v; want 0", n, err)
	}
}
