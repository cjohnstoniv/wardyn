// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

// The boot orphan pass's give-up after it stops a sandbox reads the folder record under the run's lock, and that read
// may never answer: it ends at heldStoreCallBound, decided as a read that failed, and the pass returns with the lock
// free. The record was final when the keep verdict read it, so the statement finds it final and the ref is cleared
// with no row. Catches: the give-up called without heldCall's bound, which holds the run's lock and boot for good.
func TestReconcileOrphanedSandbox_AHungGiveUpReadEndsAtItsBound(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	rn := &orphanSweepRunner{fakeRunner: &fakeRunner{}}
	s, st, audit, run := unreadFolderServer(t, types.RunCompleted, rn)
	st.record = types.RunLocalFolderStatus{Back: &types.FolderPassRecord{At: time.Now().UTC()}}
	st.final = true
	var once atomic.Bool
	st.hang = func() bool { return rn.stopCount("sbx") > 0 && once.CompareAndSwap(false, true) }
	lockFreesWithin(t, s, run.ID, "the boot orphan pass", func(ctx context.Context) { _ = s.reconcileOrphanedSandbox(ctx) })
	if st.hung.Load() != 1 || rn.stopCount("sbx") != 1 || !slices.Contains(st.clearedRefs, run.ID) ||
		len(st.liveGiveUps()) != 1 || endGiveUpRows(audit, run.ID, "") != 0 {
		t.Fatalf("%d hung reads, %d stops, refs cleared %v, %d give-ups, %d rows; want 1, 1, the ref, 1, 0",
			st.hung.Load(), rn.stopCount("sbx"), st.clearedRefs, len(st.liveGiveUps()), endGiveUpRows(audit, run.ID, ""))
	}
}
