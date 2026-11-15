// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tail owner's history tail of a killed run past its keep, which still holds its sandbox ref and whose stored
// folder record does not decode but holds a resume, ends with that copy back given up, its given_up row saying the
// record could not be read, and then the ref cleared. The tail owner takes the history tail for a killed run (the
// record-free checks say it owes nothing), and the stop's give-up at the end of that tail (giveUpFolderStopped)
// finds the resume without decoding the record. Catches: the tail stopping the sandbox and clearing a killed run's
// ref with its changes owed and no given_up row.
func TestTailOwner_AKilledRunsUnreadableRecordHoldingAResumeIsGivenUpAtTheEndOfItsTail(t *testing.T) {
	s, cs, audit, run, sb, refAt := endGiveUpFixture(t, types.RunKilled, keptForCopyBack())
	down := &atomic.Bool{}
	down.Store(true)
	s.cfg.Store = unreadableAfterStopStore{refAtEndGiveUpStore: s.cfg.Store.(refAtEndGiveUpStore), down: down}
	ended := time.Now().UTC().Add(-s.pendingCap() - time.Hour)
	cs.dispatchTestStore.mu.Lock()
	cs.run.EndedAt = &ended
	cs.dispatchTestStore.mu.Unlock()
	cs.giveUpApplies = true
	if !s.tailRun(context.Background(), run.ID, tailHistory) {
		t.Fatal("the tail owner did not run the history tail for the killed run past its keep")
	}
	cur, _ := cs.GetRun(context.Background(), run.ID)
	rows := endGiveUpRows(audit, run.ID, "status_unreadable")
	if sb.stopCount() != 1 || len(cs.giveUps) != 1 || rows != 1 || cur.SandboxRef != "" {
		t.Fatalf("%d stops, %d give-ups, %d given_up rows, ref %q; want 1 stop, 1 give-up, 1 row and the ref cleared",
			sb.stopCount(), len(cs.giveUps), rows, cur.SandboxRef)
	}
	if slices.ContainsFunc(*refAt, func(r string) bool { return r != "sbx" }) {
		t.Errorf("refs at give-up %v; want the give-up made while the ref was set", *refAt)
	}
	if p := cs.giveUps[0]; p.Cap != s.pendingCap() || p.D1 || !strings.HasSuffix(p.Back.Error, folderStatusUnreadable) {
		t.Errorf("give-up %+v; want the keep as its cap, no containment terms, and the unreadable record noted", p)
	}
}
