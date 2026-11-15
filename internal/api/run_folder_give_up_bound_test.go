// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// hungGiveUpLab is tailLab whose give-up statement never answers: it waits for its context to end, as a statement
// stuck behind a row lock another transaction holds does. The run and its record are read as tailLab reads them.
type hungGiveUpLab struct {
	*tailLab
	calls *atomic.Int32
}

func (l hungGiveUpLab) GiveUpRunLocalFolder(ctx context.Context, _ uuid.UUID, _ store.GiveUpParams) (store.GiveUpOutcome, error) {
	l.calls.Add(1)
	<-ctx.Done()
	return store.GiveUpOutcome{}, ctx.Err()
}

// TestTailOwner_AnAwayGiveUpThatNeverAnswersFreesTheRunLock: the tail owner's give-up for a runner that is away holds
// the run's lock on the cleanup loop's context, which has no deadline. Its re-read of the run and the record answers,
// and its statement then never does. The give-up ends at heldStoreCallBound: the tick returns with the run's lock
// free, and the next tick, the statement answering, gives the copy back up with its row. Catches: a bound on the
// re-read alone, which leaves the statement and its audit row holding the lock and the loop.
func TestTailOwner_AnAwayGiveUpThatNeverAnswersFreesTheRunLock(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	g := newTailRig(t)
	g.lab.giveUpOK = true
	id := g.lab.runner.ID
	row := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
	row.run.RunnerID = &id
	g.online.Store(false)
	var hung atomic.Int32
	g.s.cfg.Store = hungGiveUpLab{tailLab: g.lab, calls: &hung}
	lockFreesWithin(t, g.s, row.run.ID, "the tail owner's tick", func(ctx context.Context) { g.s.folderCleanupTick(ctx) })
	if hung.Load() != 1 || g.lab.giveUpCount() != 0 {
		t.Fatalf("%d hung statements, %d answered give-ups; want the statement reached once, under the lock", hung.Load(), g.lab.giveUpCount())
	}
	g.s.cfg.Store = g.lab
	g.tick()
	if g.lab.giveUpCount() != 1 || transferAudit(g.audit) != 1 {
		t.Fatalf("%d give-ups, %d given_up rows on the next tick; want 1, 1", g.lab.giveUpCount(), transferAudit(g.audit))
	}
}
