// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"
)

// A runner away from every server, and a lock never let go: a forced-marked row past the cap is given up without
// the lock, with its one given_up row, since its forced end already went past a holder that did not let go; a row
// that is not marked still waits for its lock, since a pass on another server may hold it. Catches: the marked
// give-up kept behind the lock, which shows the changes as kept for good; and the lock dropped for every away row,
// which can give up a copy back that a pass is still bringing back.
func TestTailOwner_AnAwayForcedMarkedRowIsGivenUpWhileItsLockIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marked  bool
		giveUps int
	}{
		{"forced-marked", true, 1},
		{"not marked", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTailRig(t)
			g.lab.giveUpOK = true
			id := g.lab.runner.ID
			row := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
			row.run.RunnerID = &id
			if tc.marked {
				at := g.clock.now
				row.forced = &at
			}
			g.online.Store(false)
			_, unlock, ok := g.s.tryLockRunOp(context.Background(), row.run.ID)
			if !ok {
				t.Fatal("the test could not take the run's lock")
			}
			defer unlock()
			g.tick()
			g.tick()
			if g.lab.giveUpCount() != tc.giveUps || transferAudit(g.audit) != tc.giveUps || g.total() != 0 {
				t.Errorf("%d give-ups, %d given_up rows, %d tails over two ticks with the lock held; want %d, %d and 0",
					g.lab.giveUpCount(), transferAudit(g.audit), g.total(), tc.giveUps, tc.giveUps)
			}
		})
	}
}
