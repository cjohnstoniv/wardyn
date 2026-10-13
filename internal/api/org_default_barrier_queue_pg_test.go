// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation's default barrier and the pool queue, against a real Postgres: guarded by
// WARDYN_TEST_PG, skipped cleanly when unset, must PASS when set.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run that waited for a slot keeps the barrier its create resolved. The create stamps who chose it
// (run.create confinement_default, on the queued row too); lowering the organisation's default while the
// run waits does not make the start pick again and end the run as queue_policy_changed: it starts under
// its original class, and run.queue.start records the class and who chose it.
func TestOrgDefaultBarrierQueuedRunKeepsItsClass(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	e.setOrgBarrier("CC3")
	waiting := e.queuedLaunch(nil, pool, "")
	if waiting.ConfinementClass != types.CC3 {
		t.Fatalf("the queued run holds %s, want the organisation's CC3", waiting.ConfinementClass)
	}
	created := e.audits(waiting.ID, "run.create")
	if len(created) != 1 || queueAuditData(t, created[0])["confinement_default"] != "organisation" {
		t.Fatalf("the queued run.create rows = %+v, want one stamped confinement_default organisation", created)
	}

	e.setOrgBarrier("CC2")
	e.free(occupant)
	started := e.waitStarted(waiting.ID)
	if started.ConfinementClass != types.CC3 || started.State == types.RunFailed {
		t.Fatalf("the run started as %s %s, want to keep CC3 after the organisation lowered its default", started.State, started.ConfinementClass)
	}
	data := queueAuditData(t, e.waitAudit(waiting.ID, "run.queue.start")[0])
	if data["confinement_class"] != "CC3" || data["confinement_default"] != "organisation" {
		t.Errorf("run.queue.start = %v, want confinement_class CC3 chosen by the organisation", data)
	}
}

// The organisation raising its default while a run waits tightens the run: the start takes the stronger
// class when the pool offers it, and says so on run.queue.start. A raise to a class the pool does not
// offer leaves the run's own.
func TestOrgDefaultBarrierQueuedRunTakesARaisedDefault(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	e.setOrgBarrier("CC2")
	waiting := e.queuedLaunch(nil, pool, "")
	if waiting.ConfinementClass != types.CC2 {
		t.Fatalf("the queued run holds %s, want the organisation's CC2", waiting.ConfinementClass)
	}
	e.setOrgBarrier("CC3")
	e.free(occupant)
	started := e.waitStarted(waiting.ID)
	if started.ConfinementClass != types.CC3 {
		t.Fatalf("the run started as %s, want the raised default CC3", started.ConfinementClass)
	}
	data := queueAuditData(t, e.waitAudit(waiting.ID, "run.queue.start")[0])
	if data["confinement_class"] != "CC3" || data["confinement_default"] != "organisation" || data["confinement_default_raised"] != true {
		t.Errorf("run.queue.start = %v, want CC3 chosen by the organisation, raised", data)
	}
}

func TestOrgDefaultBarrierQueuedRunIgnoresARaisedDefaultThePoolDoesNotOffer(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	e.setOrgBarrier("CC2")
	waiting := e.queuedLaunch(nil, pool, "")
	e.setOrgBarrier("CC3")
	e.runner.execFacts["alpha"] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2}}
	e.free(occupant)
	started := e.waitStarted(waiting.ID)
	if started.ConfinementClass != types.CC2 || started.State == types.RunFailed {
		t.Fatalf("the run started as %s %s, want to keep CC2 when the pool does not offer CC3", started.State, started.ConfinementClass)
	}
	if data := queueAuditData(t, e.waitAudit(waiting.ID, "run.queue.start")[0]); data["confinement_default_raised"] != nil {
		t.Errorf("run.queue.start = %v, want no confinement_default_raised", data)
	}
}

// R2 L1: a launch that waits records on its run.create row the drives and the harness set it attaches,
// as the direct create does.
func TestQueuedCreateAuditStampsDrivesAndHarnesses(t *testing.T) {
	e := newQueueEnv(t)
	id := uuid.New()
	var f runFold
	f.drives = []runDrive{{id: "d1", target: "/work", resolved: types.ResolvedDrive{Drive: types.UserDrive{Name: "team"}}}}
	f.harnesses = []types.RunHarness{{Harness: "claude-code", ModelProviderID: "p1", Starts: true}}
	created := types.AgentRun{ID: id, CreatedBy: "sub-person", ConfinementClass: types.CC1}
	e.srv.auditQueuedCreate(context.Background(), queuedCreate{req: createRunRequest{Agent: "claude-code"}, f: f, createdByType: types.ActorHuman}, created, store.RunQueueEntry{}, 60, 1)
	rows := e.audits(id, "run.create")
	if len(rows) != 1 {
		t.Fatalf("run.create rows = %d, want 1", len(rows))
	}
	data := queueAuditData(t, rows[0])
	drives, _ := data["drives"].([]any)
	harnesses, _ := data["harnesses"].([]any)
	if len(drives) != 1 || drives[0].(map[string]any)["drive_id"] != "d1" || len(harnesses) != 1 || harnesses[0].(map[string]any)["id"] != "claude-code" {
		t.Errorf("queued run.create = %v, want the drive and the harness stamped", data)
	}
}
