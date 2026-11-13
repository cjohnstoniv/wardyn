// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// A queued run whose launch was authorised under a namespace binding starts while the binding stands. Once
// the binding is deleted, the next such run never starts: it ends FAILED workload_binding_inactive with the
// binding-gone sentence, on a run.queue.start failure row.
func TestPG_QueueStartOfABoundRunWhoseBindingWasDeleted(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 2)
	first, second := e.occupy(pool), e.occupy(pool)
	standing := e.queuedLaunch(e.person, pool, "")
	gone := e.queuedLaunch(e.person, pool, "")
	var binding uuid.UUID
	if err := e.poolEnv.pool.QueryRow(context.Background(), `INSERT INTO workload_bindings (pool_id, namespace, service, sponsor, created_by)
		VALUES ($1, 'team-a', 'bot', 'sponsor', 'secop') RETURNING id`, pool.ID).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{standing.ID, gone.ID} {
		e.exec(`UPDATE run_queue SET authority = authority || jsonb_build_object('binding', $2::text) WHERE run_id = $1`, id, binding.String())
	}
	e.free(first)
	e.waitStarted(standing.ID)

	if _, err := e.pg.DeleteWorkloadBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	e.free(second)
	got := e.waitFailed(gone.ID)
	if got.StatusDetail != statusDetailQueuePrefix+reasonWorkloadBindingInactive || got.FailureHint != workloadBindingGoneMsg || got.SPIFFEID != "" {
		t.Errorf("a run whose binding was deleted while it waited = %s %q %q %q; want FAILED %s with the binding-gone sentence, never started",
			got.State, got.StatusDetail, got.FailureHint, got.SPIFFEID, reasonWorkloadBindingInactive)
	}
	fail := e.waitAudit(gone.ID, "run.queue.start")
	if len(fail) != 1 || fail[0].Outcome != "failure" || queueAuditData(t, fail[0])["reason"] != reasonWorkloadBindingInactive {
		t.Errorf("the start's rows = %+v; want one failure with reason %s", fail, reasonWorkloadBindingInactive)
	}
}
