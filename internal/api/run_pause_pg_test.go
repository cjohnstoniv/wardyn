// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A pause's compensation against a real Postgres run lock: only a real session
// can lose its lock while the work it guards is in flight. Leader A and leader
// B are two servers, each with its own locker over one database, i.e. two
// replicas. The run's freeze state is the runner double's.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// frozenRunner is pauseRunner that remembers whether the sandbox is frozen and,
// like a real runtime call, refuses a call whose context has ended. onThaw runs
// once, before the first thaw lands. failFreezes fails that many freezes before
// they reach the sandbox.
type frozenRunner struct {
	*pauseRunner
	frozen      atomic.Bool
	onThaw      func()
	failFreezes atomic.Int32
}

func (r *frozenRunner) FreezeSandbox(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.failFreezes.Add(-1) >= 0 {
		return errors.New("injected: the runtime refused the freeze")
	}
	r.failFreezes.Store(0)
	if err := r.pauseRunner.FreezeSandbox(ctx, ref); err != nil {
		return err
	}
	r.frozen.Store(true)
	return nil
}

func (r *frozenRunner) ThawSandbox(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fn := r.onThaw; fn != nil {
		r.onThaw = nil
		fn()
	}
	if err := r.pauseRunner.ThawSandbox(ctx, ref); err != nil {
		return err
	}
	r.frozen.Store(false)
	return nil
}

// faultStore is the pause store A reads through: afterRead runs once, after a
// read and before it returns, and failReads fails that many reads before they
// reach the row.
type faultStore struct {
	*pauseStore
	afterRead func()
	failReads atomic.Int32
}

func (h *faultStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if h.failReads.Add(-1) >= 0 {
		return types.AgentRun{}, errors.New("injected: the run read failed")
	}
	h.failReads.Store(0)
	run, err := h.pauseStore.GetRun(ctx, id)
	if fn := h.afterRead; fn != nil {
		h.afterRead = nil
		fn()
	}
	return run, err
}

// pgPauseLeaders is the pause fixture with its run lock in Postgres: a, the
// leader whose pass ctx loseLeader ends, and b, a second replica whose pass
// carries the next epoch. a's freeze supersedes its epoch, so a's mark is
// refused and its compensation runs.
type pgPauseLeaders struct {
	f          *pauseFixture
	pool       *pgxpool.Pool
	rn         *frozenRunner
	hook       *faultStore
	lease      *fakeLease
	b          *Server
	ctxA, ctxB context.Context
	loseLeader context.CancelFunc
}

func newPGPauseLeaders(t *testing.T) *pgPauseLeaders {
	t.Helper()
	pool := throwawayPGPool(t)
	var newest atomic.Int64
	newest.Store(1)
	lease := &fakeLease{epoch: 2, termCtx: context.Background(), current: func(epoch int64) bool { return epoch == newest.Load() }}
	f := newPauseFixture(t, time.Hour, withLease(lease))
	f.st.open, f.st.waiting = true, true
	rn := &frozenRunner{pauseRunner: f.rn}
	f.srv.cfg.Runner = rn
	f.srv.locks.override = db.NewPGLocker(pool, 2)
	ctxA, loseLeader := context.WithCancel(context.WithValue(t.Context(), leaseEpochKey{}, int64(1)))
	t.Cleanup(loseLeader)
	var first atomic.Bool
	f.rn.onFreeze = func() {
		if !first.Swap(true) {
			newest.Store(2) // B is elected while A's freeze is in flight
		}
	}
	hook := &faultStore{pauseStore: f.st}
	f.srv.cfg.Store = hook
	b := New(f.srv.cfg)
	b.cfg.Store = f.st
	b.locks.override = db.NewPGLocker(pool, 2)
	return &pgPauseLeaders{f: f, pool: pool, rn: rn, hook: hook, lease: lease, b: b,
		ctxA: ctxA, ctxB: context.WithValue(t.Context(), leaseEpochKey{}, int64(2)), loseLeader: loseLeader}
}

// loseRunLock ends the session holding the run's lock, as a failover or an
// operator's pg_terminate_backend does, and waits until the lock is free.
func (l *pgPauseLeaders) loseRunLock(t *testing.T) {
	t.Helper()
	k := db.NewLockKey(db.RunOpLockClass, l.f.run.ID.String())
	const held = `FROM pg_locks WHERE locktype = 'advisory' AND objsubid = 2 AND granted AND classid::bigint = $1 AND objid::bigint = $2`
	var killed int
	if err := l.pool.QueryRow(t.Context(), `SELECT count(*) FROM (SELECT pg_terminate_backend(pid) `+held+`) t`,
		int64(uint32(k.Class)), int64(uint32(k.Obj))).Scan(&killed); err != nil {
		t.Fatal(err)
	}
	if killed != 1 {
		t.Fatalf("terminated %d sessions holding the run lock, want 1", killed)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var n int
		if err := l.pool.QueryRow(t.Context(), `SELECT count(*) `+held, int64(uint32(k.Class)), int64(uint32(k.Obj))).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the run lock was still held after its session was terminated")
		}
	}
}

// bPauses runs B's whole pause and requires it to commit.
func (l *pgPauseLeaders) bPauses(t *testing.T) {
	t.Helper()
	l.b.pauseRun(l.ctxB, l.f.st, l.f.run, types.PauseWaiting, time.Hour)
	if pausedAt, _ := l.f.st.paused(); pausedAt == nil {
		t.Fatal("B did not mark the run paused")
	}
}

// TestPG_PauseRun_CompensationNeverUndoesANewerPause: leader A freezes, loses
// the election (its pass's context ends) and has its mark refused. Its
// compensation must thaw an unmarked freeze, and must never thaw a pause B
// committed after A's run lock went with its session, however the pass's
// cancellation and the lock's loss race. In every case the final state is
// consistent: a run marked paused is frozen, a run not marked is not.
func TestPG_PauseRun_CompensationNeverUndoesANewerPause(t *testing.T) {
	t.Run("the pass is cancelled and the lock held: the unmarked freeze is undone", func(t *testing.T) {
		l := newPGPauseLeaders(t)
		inner := l.f.rn.onFreeze
		l.f.rn.onFreeze = func() { inner(); l.loseLeader() }

		l.f.srv.pauseRun(l.ctxA, l.f.st, l.f.run, types.PauseWaiting, time.Hour)

		freezes, thaws := l.rn.counts()
		pausedAt, _ := l.f.st.paused()
		t.Logf("freezes=%d thaws=%d paused=%t frozen=%t", freezes, thaws, pausedAt != nil, l.rn.frozen.Load())
		if pausedAt != nil || freezes != 1 || thaws != 1 || l.rn.frozen.Load() {
			t.Error("want an unmarked run thawed again: freezes == thaws == 1, not paused, not frozen")
		}
		if n := l.f.st.owing(); n != 0 {
			t.Errorf("%d settles left recorded after a settled compensation, want 0", n)
		}
	})

	t.Run("the pass is cancelled, then the lock is lost before the thaw: B's pause stands", func(t *testing.T) {
		l := newPGPauseLeaders(t)
		inner := l.f.rn.onFreeze
		l.f.rn.onFreeze = func() { inner(); l.loseLeader() }
		// The cancellation has already fixed the cause of A's lock context, so
		// the lock's loss after it never shows there.
		l.hook.afterRead = func() { l.loseRunLock(t); l.bPauses(t) }

		l.f.srv.pauseRun(l.ctxA, l.f.st, l.f.run, types.PauseWaiting, time.Hour)

		freezes, thaws := l.rn.counts()
		pausedAt, _ := l.f.st.paused()
		t.Logf("freezes=%d thaws=%d paused=%t frozen=%t", freezes, thaws, pausedAt != nil, l.rn.frozen.Load())
		if pausedAt == nil || freezes != 2 || thaws != 0 || !l.rn.frozen.Load() {
			t.Error("A's stale compensation thawed B's committed pause: want freezes 2, thaws 0, paused and frozen")
		}
		if n := l.f.st.owing(); n != 0 {
			t.Errorf("%d settles left recorded after a settled compensation, want 0", n)
		}
	})

	t.Run("the lock is lost while the thaw is in flight: B's pause is put back", func(t *testing.T) {
		l := newPGPauseLeaders(t)
		// B freezes and marks before A's thaw lands.
		l.rn.onThaw = func() { l.loseRunLock(t); l.bPauses(t) }

		l.f.srv.pauseRun(l.ctxA, l.f.st, l.f.run, types.PauseWaiting, time.Hour)

		freezes, thaws := l.rn.counts()
		pausedAt, _ := l.f.st.paused()
		t.Logf("freezes=%d thaws=%d paused=%t frozen=%t", freezes, thaws, pausedAt != nil, l.rn.frozen.Load())
		if pausedAt == nil || !l.rn.frozen.Load() {
			t.Errorf("the run is marked paused=%t with its sandbox frozen=%t after A's thaw raced B's pause; want both", pausedAt != nil, l.rn.frozen.Load())
		}
		if freezes != 3 || thaws != 1 {
			t.Errorf("freezes, thaws = %d, %d; want 3, 1: A's freeze, B's, and A restoring B's under a new lock", freezes, thaws)
		}
		if n := l.f.st.owing(); n != 0 {
			t.Errorf("%d settles left recorded after a settled compensation, want 0", n)
		}
	})
}

// state is the run's pause mark and its sandbox, as the test reads them.
func (l *pgPauseLeaders) state() (paused, frozen bool, freezes, thaws int) {
	pausedAt, _ := l.f.st.paused()
	freezes, thaws = l.rn.counts()
	return pausedAt != nil, l.rn.frozen.Load(), freezes, thaws
}

// leaderSweeps runs the pause sweep as the current leader once every recorded
// settle is due.
func (l *pgPauseLeaders) leaderSweeps(t *testing.T) {
	t.Helper()
	l.lease.mu.Lock()
	l.lease.lead = true
	l.lease.mu.Unlock()
	l.f.st.settleDue()
	l.f.sweep(t)
}

// TestPG_PauseRun_CompensationFailsClosedAndTheSweepSettles: a compensation
// whose read of the run, or whose repair of the sandbox, fails never thaws on
// what it could not read and never abandons the repair. A one-off failure is
// tried again in the round; one that persists leaves the sandbox as it is with
// the run's settle recorded, and the next leader sweep settles it under the
// run lock: a run marked paused ends frozen, one not marked ends running.
func TestPG_PauseRun_CompensationFailsClosedAndTheSweepSettles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fails int32
	}{{"once", 1}, {"every try", pauseSettleTries}} {
		t.Run("the run cannot be read after the lock is taken again, "+tc.name+": B's pause is never thawed", func(t *testing.T) {
			l := newPGPauseLeaders(t)
			// A read the run unpaused; its lock goes and B pauses; the read under
			// A's new lock fails.
			l.hook.afterRead = func() { l.loseRunLock(t); l.bPauses(t); l.hook.failReads.Store(tc.fails) }

			l.f.srv.pauseRun(l.ctxA, l.f.st, l.f.run, types.PauseWaiting, time.Hour)

			paused, frozen, freezes, thaws := l.state()
			t.Logf("after A: freezes=%d thaws=%d paused=%t frozen=%t owing=%d", freezes, thaws, paused, frozen, l.f.st.owing())
			if thaws != 0 || !paused || !frozen {
				t.Fatalf("A thawed on a read it could not make: want thaws 0, paused and frozen")
			}
			if want := map[bool]int{true: 1, false: 0}[tc.fails == pauseSettleTries]; l.f.st.owing() != want {
				t.Errorf("settles recorded after A = %d, want %d", l.f.st.owing(), want)
			}
			l.leaderSweeps(t)
			paused, frozen, freezes, thaws = l.state()
			t.Logf("after the sweep: freezes=%d thaws=%d paused=%t frozen=%t owing=%d", freezes, thaws, paused, frozen, l.f.st.owing())
			if thaws != 0 || !paused || !frozen || l.f.st.owing() != 0 {
				t.Error("after the sweep: want B's pause frozen, never thawed, and nothing left to settle")
			}
		})
	}

	for _, tc := range []struct {
		name  string
		fails int32
	}{{"once", 1}, {"every try", pauseSettleTries}} {
		t.Run("the freeze that puts B's pause back fails "+tc.name+": it is retried, else the sweep settles it", func(t *testing.T) {
			l := newPGPauseLeaders(t)
			// B freezes and marks before A's thaw lands; A's repair freeze then fails.
			l.rn.onThaw = func() { l.loseRunLock(t); l.bPauses(t); l.rn.failFreezes.Store(tc.fails) }

			l.f.srv.pauseRun(l.ctxA, l.f.st, l.f.run, types.PauseWaiting, time.Hour)

			paused, frozen, freezes, thaws := l.state()
			persists := tc.fails == pauseSettleTries
			t.Logf("after A: freezes=%d thaws=%d paused=%t frozen=%t owing=%d", freezes, thaws, paused, frozen, l.f.st.owing())
			if !paused || frozen == persists {
				t.Errorf("after A: paused=%t frozen=%t; want paused, frozen unless every try failed", paused, frozen)
			}
			if want := map[bool]int{true: 1, false: 0}[persists]; l.f.st.owing() != want {
				t.Errorf("settles recorded after A = %d, want %d: a failed repair must stay recorded", l.f.st.owing(), want)
			}
			l.leaderSweeps(t)
			paused, frozen, freezes, thaws = l.state()
			t.Logf("after the sweep: freezes=%d thaws=%d paused=%t frozen=%t owing=%d", freezes, thaws, paused, frozen, l.f.st.owing())
			if !paused || !frozen || l.f.st.owing() != 0 {
				t.Error("after the sweep: want the run marked paused, its sandbox frozen, and nothing left to settle")
			}
		})
	}

	t.Run("the run cannot be read before the thaw: nothing thaws, and the sweep thaws the unmarked freeze", func(t *testing.T) {
		l := newPGPauseLeaders(t)
		l.hook.failReads.Store(pauseSettleTries)

		l.f.srv.pauseRun(l.ctxA, l.f.st, l.f.run, types.PauseWaiting, time.Hour)

		paused, frozen, freezes, thaws := l.state()
		t.Logf("after A: freezes=%d thaws=%d paused=%t frozen=%t owing=%d", freezes, thaws, paused, frozen, l.f.st.owing())
		if thaws != 0 || !frozen || paused || l.f.st.owing() != 1 {
			t.Fatal("after A: want no thaw on a row nobody read, the freeze left as it is and its settle recorded")
		}
		// The request has closed, so the sweep pauses nothing anew and its only work is the settle.
		l.f.st.mu.Lock()
		l.f.st.open, l.f.st.waiting = false, false
		l.f.st.mu.Unlock()
		l.leaderSweeps(t)
		paused, frozen, freezes, thaws = l.state()
		t.Logf("after the sweep: freezes=%d thaws=%d paused=%t frozen=%t owing=%d", freezes, thaws, paused, frozen, l.f.st.owing())
		if paused || frozen || freezes != thaws || l.f.st.owing() != 0 {
			t.Error("after the sweep: want the unmarked run running again (freezes == thaws) and nothing left to settle")
		}
	})
}
