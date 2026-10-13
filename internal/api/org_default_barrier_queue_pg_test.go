// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation's default barrier and the pool queue, against a real Postgres: guarded by
// WARDYN_TEST_PG, skipped cleanly when unset, must PASS when set.

import (
	"testing"

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
