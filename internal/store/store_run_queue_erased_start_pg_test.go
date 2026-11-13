// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The start of a promoted run its owner's run_tasks erasure marked writes nothing and answers
// ErrRunTasksErased; once a kill has won the same run, the start answers a plain refusal, as for any
// moved run. Catches a start that writes a marked run, and one that reads every refusal as the mark.
func TestPG_QueueStartOfARunMarkedByTheTaskErasureIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	queued, err := enqueue(t, pg, p, "alice", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll); err != nil || len(got) != 1 {
		t.Fatalf("promote: %v %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET tasks_erased_at = now() WHERE id = $1`, queued.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := pg.StartQueuedRun(ctx, queued.ID, store.QueueStart{SPIFFEID: "spiffe://x/marked", ConfinementClass: types.CC2}); ok || !errors.Is(err, store.ErrRunTasksErased) {
		t.Errorf("the start of a marked run: ok=%v err=%v; want ErrRunTasksErased", ok, err)
	}
	if got := runState(t, pg, queued.ID); got.State != types.RunPending || got.SPIFFEID != "" {
		t.Errorf("the marked run after its refused start = %s spiffe %q; want PENDING with nothing written", got.State, got.SPIFFEID)
	}
	if ok, err := pg.UpdateRunStateIf(ctx, queued.ID, types.RunPending, types.RunKilled); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := pg.StartQueuedRun(ctx, queued.ID, store.QueueStart{SPIFFEID: "spiffe://x/late"}); err != nil || ok {
		t.Errorf("the start of a marked run a kill had won: ok=%v err=%v; want a plain refusal", ok, err)
	}
}
