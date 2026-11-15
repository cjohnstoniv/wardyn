// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// restopFixture is a forced run whose sandbox stop succeeds and whose cut is never confirmed (its proxy stop fails),
// with the lock free and the run's data_out rule unreadable, so inside the keep every stop leaves the copy back not
// given up. It counts the run's revokes.
func restopFixture(t *testing.T) (*forced, *togglingRevokeBroker) {
	t.Helper()
	f := newForced(t)
	b := &togglingRevokeBroker{}
	f.s.cfg.Broker = b
	f.claimOnly(t)
	f.release()
	f.setRecord(types.RunLocalFolderStatus{Gen: 3, Settled: 3})
	f.cs.policyErr = errors.New("the audit read failed")
	f.sb.proxyErr = errors.New("daemon unavailable")
	return f, b
}

// Inside the keep, a sandbox the forced retry stopped whose copy back could not be given up is stopped again only
// once this server's wait has run out: 2 minutes, doubling to 10. Over 30 one-minute ticks it is stopped on ticks 1,
// 3, 7, 15 and 25, the revoke runs on every tick, and the mark stays. Catches: a stopped sandbox stopped again, with
// its warning, on every tick for the whole keep; a wait that also holds the revoke back.
func TestSweepForcedStops_AStoppedSandboxIsStoppedAgainOnlyOnceItsWaitRunsOut(t *testing.T) {
	f, b := restopFixture(t)
	var stoppedOn []int
	for i := 1; i <= 30; i++ {
		stops, revokes := f.sb.stopCount(), b.calls.Load()
		f.tick(t, f.s)
		if f.sb.stopCount() > stops {
			stoppedOn = append(stoppedOn, i)
		}
		if b.calls.Load() == revokes {
			t.Errorf("tick %d ran no revoke; the revoke runs on every tick", i)
		}
	}
	if want := []int{1, 3, 7, 15, 25}; !slices.Equal(stoppedOn, want) || f.leaser.markCount() != 1 {
		t.Errorf("stopped on ticks %v, %d marks; want %v and the mark kept", stoppedOn, f.leaser.markCount(), want)
	}
}

// The wait is this server's own: another server's retry of the same run stops it once and then waits too. Catches: a
// wait that one server's stop imposes on the others, which holds a restarted or second replica's first stop back.
func TestSweepForcedStops_AnotherServersRetryStopsOnceThenWaits(t *testing.T) {
	f, _ := restopFixture(t)
	other := New(f.s.cfg)
	other.locks.override = f.s.locker()
	f.tick(t, f.s)
	f.tick(t, other)
	f.tick(t, other)
	if f.sb.stopCount() != 2 {
		t.Errorf("%d stops; want 2: one by each server, and none by the other server inside its own wait", f.sb.stopCount())
	}
}

// Past the keep, the give-up is not held by the wait: on the next tick the owed copy back is given up with its row,
// while the sandbox waits to be stopped again; once the server's wait runs out, the stop clears the ref and the mark.
// The server's clock is apart from the database's, so the keep runs out inside the wait. Catches: a wait that delays
// the give-up past the keep, which keeps the changes shown as kept after their keep has run out.
func TestSweepForcedStops_TheGiveUpPastTheKeepIsNotHeldByTheWait(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	f.release()
	f.setRecord(owedCounted())
	f.sb.proxyErr = errors.New("daemon unavailable") // the cut is not confirmed, so the sandbox is stopped
	srv := &keptEndClock{now: f.clock.now}
	f.s.cfg.Now = srv.Now
	f.tick(t, f.s) // inside the keep: stopped, the give-up does not apply
	if got := f.giveUpState(t); got != (forcedGiveUpState{giveUps: 1, marks: 1}) || f.sb.stopCount() != 1 {
		t.Fatalf("inside the keep: %+v, %d stops; want one give-up that did not apply, the mark kept and one stop", got, f.sb.stopCount())
	}
	f.pastTheKeep()
	f.cs.giveUpApplies = true
	f.tick(t, f.s) // the database is past the keep; the server's wait has not run out
	if got := f.giveUpState(t); got.giveUps != 2 || got.rows != 1 || got.cleared || got.marks != 1 || f.sb.stopCount() != 1 {
		t.Fatalf("past the keep, inside the wait: %+v, %d stops; want the copy back given up with its row, no stop, the ref and the mark kept", got, f.sb.stopCount())
	}
	srv.now = srv.now.Add(forcedRestopWaitMin)
	f.tick(t, f.s)
	if got := f.giveUpState(t); got.rows != 1 || !got.cleared || got.marks != 0 || got.success != 1 || f.sb.stopCount() != 2 {
		t.Errorf("past the wait: %+v, %d stops; want the sandbox stopped, the ref and the mark cleared with one success row", got, f.sb.stopCount())
	}
}
