// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeLease is a SweeperLease with a scripted leader: lead says whether Join
// joins a term, current answers the epoch fence.
type fakeLease struct {
	mu           sync.Mutex
	lead         bool
	termCtx      context.Context
	epoch        int64
	current      func(epoch int64) bool
	begins, ends int
}

func (l *fakeLease) Join() (context.Context, int64, func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.lead {
		return nil, 0, nil, false
	}
	l.begins++
	return l.termCtx, l.epoch, func() { l.mu.Lock(); l.ends++; l.mu.Unlock() }, true
}

func (l *fakeLease) Current(ctx context.Context, epoch int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err // the epoch read is a database call, which a cancelled pass cannot make
	}
	return l.current(epoch), nil
}

func (l *fakeLease) counts() (begins, ends int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.begins, l.ends
}

func withLease(l *fakeLease) func(*Config) { return func(c *Config) { c.SweeperLease = l } }

// TestPauseRun_TwoStaleLeadersLeaveTheRunFrozenAndMarked is the interleaving a
// failover produces: two leaders both freeze the run, one marks it paused, the
// other loses the compare. The loser's compensation used to thaw the sandbox,
// undoing the winner while the database said paused. Idempotence does not save
// it (the thaw is idempotent and still wrong), so the schedule is run
// explicitly, with the stale leader both detected by the epoch fence and not.
func TestPauseRun_TwoStaleLeadersLeaveTheRunFrozenAndMarked(t *testing.T) {
	for _, tc := range []struct {
		name string
		// supersede: the first leader's epoch is stale by the time it marks.
		supersede bool
	}{
		{"the loser still believes it leads", false},
		{"the loser has been superseded", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var newest atomic.Int64
			newest.Store(1)
			lease := &fakeLease{current: func(epoch int64) bool {
				return !tc.supersede || epoch == newest.Load()
			}}
			f := newPauseFixture(t, time.Hour, withLease(lease))
			f.st.open, f.st.waiting = true, true

			ctxA := context.WithValue(context.Background(), leaseEpochKey{}, int64(1))
			ctxB := context.WithValue(context.Background(), leaseEpochKey{}, int64(2))
			var nested atomic.Bool
			f.rn.onFreeze = func() {
				// Leader A has frozen the sandbox and not yet marked it. The new
				// leader B runs its whole pause now, and wins the mark.
				if nested.Swap(true) {
					return
				}
				newest.Store(2)
				f.srv.pauseRun(ctxB, f.st, f.run, types.PauseWaiting, time.Hour)
			}

			f.srv.pauseRun(ctxA, f.st, f.run, types.PauseWaiting, time.Hour)

			if pausedAt, _ := f.st.paused(); pausedAt == nil {
				t.Fatal("the run is not marked paused")
			}
			freezes, thaws := f.rn.counts()
			if freezes != 2 {
				t.Errorf("freezes = %d, want 2 (both leaders froze)", freezes)
			}
			if thaws != 0 {
				t.Errorf("thaws = %d, want 0: the stale leader's compensation undid the winner's pause", thaws)
			}
			if got := len(f.rows("run.pause", "success")); got != 1 {
				t.Errorf("run.pause success rows = %d, want 1", got)
			}
		})
	}
}

// TestPauseRun_ASupersededLeaderStartsNothing: a leader whose epoch is no longer
// current does not freeze at all, and one that is superseded after it froze
// still thaws a run nobody marked (the mark never happened, so nothing holds it).
func TestPauseRun_ASupersededLeaderStartsNothing(t *testing.T) {
	var stale atomic.Bool
	lease := &fakeLease{current: func(int64) bool { return !stale.Load() }}
	f := newPauseFixture(t, time.Hour, withLease(lease))
	f.st.open, f.st.waiting = true, true
	ctx := context.WithValue(context.Background(), leaseEpochKey{}, int64(1))

	stale.Store(true)
	f.srv.pauseRun(ctx, f.st, f.run, types.PauseWaiting, time.Hour)
	if freezes, _ := f.rn.counts(); freezes != 0 {
		t.Errorf("a superseded leader froze the sandbox %d times", freezes)
	}

	stale.Store(false)
	f.rn.onFreeze = func() { stale.Store(true) }
	f.srv.pauseRun(ctx, f.st, f.run, types.PauseWaiting, time.Hour)
	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Error("a superseded leader marked the run paused")
	}
	if freezes, thaws := f.rn.counts(); freezes != 1 || thaws != 1 {
		t.Errorf("freezes, thaws = %d, %d; want 1, 1: the unmarked freeze must be undone", freezes, thaws)
	}
}

// TestSweepRunPauses_OnlyTheLeaderSweeps: a follower's pass does nothing, a
// leader's pass pauses the run and ends its term membership, and a leader whose
// lease ends mid-pass has its context cancelled.
func TestSweepRunPauses_OnlyTheLeaderSweeps(t *testing.T) {
	termCtx, lose := context.WithCancel(context.Background())
	defer lose()
	lease := &fakeLease{termCtx: termCtx, epoch: 7, current: func(epoch int64) bool { return epoch == 7 }}
	f := newPauseFixture(t, time.Hour, withLease(lease))
	f.st.open, f.st.waiting = true, true

	f.sweep(t) // follower
	if begins, _ := lease.counts(); begins != 0 {
		t.Fatalf("a follower joined a term %d times", begins)
	}
	if freezes, _ := f.rn.counts(); freezes != 0 {
		t.Fatalf("a follower froze the sandbox %d times", freezes)
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Fatal("a follower marked the run paused")
	}

	lease.mu.Lock()
	lease.lead = true
	lease.mu.Unlock()
	f.sweep(t) // leader
	if pausedAt, _ := f.st.paused(); pausedAt == nil {
		t.Fatal("the leader did not pause the run")
	}
	if begins, ends := lease.counts(); begins != 1 || ends != 1 {
		t.Errorf("begins, ends = %d, %d; want 1, 1: the pass must end its membership so the leader can join it", begins, ends)
	}
}

// TestSweepRunPauses_LeaseLossCancelsThePass: the pass's context follows the
// term's, so a lost lease stops it before the leader lets go of its lock.
func TestSweepRunPauses_LeaseLossCancelsThePass(t *testing.T) {
	termCtx, lose := context.WithCancel(context.Background())
	lease := &fakeLease{lead: true, termCtx: termCtx, epoch: 3, current: func(int64) bool { return true }}
	f := newPauseFixture(t, time.Hour, withLease(lease))
	f.st.open, f.st.waiting = true, true
	f.rn.onFreeze = func() { // the lease is lost between the freeze and the mark
		lose()
		time.Sleep(100 * time.Millisecond) // context.AfterFunc cancels the pass on its own goroutine
	}

	f.sweep(t)

	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Error("a pass whose lease was lost still marked the run paused")
	}
	if freezes, thaws := f.rn.counts(); freezes != 1 || thaws != 1 {
		t.Errorf("freezes, thaws = %d, %d; want 1, 1: the compensation must outlive the cancelled pass", freezes, thaws)
	}
}

// TestSweepRunSecrets_RunsOnAFollower: the run-secret sweep is deliberately not
// leader-gated, because it drops THIS replica's own in-memory masking corpus. A
// run terminal past RunSecretGrace whose values were loaded on a follower must
// be gone from the follower's registry, whoever leads.
func TestSweepRunSecrets_RunsOnAFollower(t *testing.T) {
	h := newHarness(t)
	cold := agedRun(types.RunCompleted, 2*RunSecretGrace)
	reg := secretmask.NewRegistry()
	reg.Add(cold.ID, []byte("follower-loaded-secret"))

	cfg := baseTestConfig(h, &sweepStore{runs: []types.AgentRun{cold}})
	cfg.MaskRegistry = reg
	cfg.SweeperLease = &fakeLease{lead: false, current: func(int64) bool { return false }}
	srv := New(cfg)

	if n := srv.SweepRunSecrets(context.Background()); n != 1 {
		t.Fatalf("evicted = %d, want 1: a follower must still drop its own cache", n)
	}
	if snap := reg.Snapshot(cold.ID); len(snap) != 0 {
		t.Errorf("the follower's registry still holds %d secret(s) of a cold terminal run", len(snap))
	}
}
