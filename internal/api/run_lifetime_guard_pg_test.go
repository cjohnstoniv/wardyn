// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A queued run whose captured lifetime cap has passed ends before anything is minted. Guarded by
// WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The promotion stamps admitted_at at the database's now and the pool caps the run two hours on, so the
// cap is passed only when the server's clock has moved on: the test moves it.
func TestQueuedRunPastLifetimeCapNotStarted(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	waiting, occupant := e.waitingPerson(pool, "")
	e.srv.cfg.Now = func() time.Time { return time.Now().UTC().Add(3 * time.Hour) }
	e.free(occupant)
	e.ended(waiting.ID, queueStartRecheck{
		reason: string(types.RunEndMaxLifetimeReached), sent: types.RunEndMaxLifetimeReached.Sentence(),
	})
}

func TestQueuedRunBeforeLifetimeCapStarts(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	waiting, occupant := e.waitingPerson(pool, "")
	e.free(occupant)
	got := e.waitStarted(waiting.ID)
	if got.LifetimeCapAt == nil || !got.LifetimeCapAt.After(time.Now()) {
		t.Errorf("a started run's lifetime_cap_at = %v, want a time ahead", got.LifetimeCapAt)
	}
}
