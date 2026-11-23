// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeRunRetentionStore answers DeleteExpiredRuns from a script, then 0, and counts the calls.
type fakeRunRetentionStore struct {
	script []int
	errAt  int // the call (1-based) that returns err after its count; 0 never
	err    error
	calls  int
}

func (f *fakeRunRetentionStore) DeleteExpiredRuns(_ context.Context, _ time.Duration, limit int) (int, error) {
	f.calls++
	n := 0
	if f.calls <= len(f.script) {
		n = f.script[f.calls-1]
	}
	if n > limit {
		n = limit
	}
	if f.errAt == f.calls {
		return n, f.err
	}
	return n, nil
}

// alwaysFullRunRetentionStore returns a full batch on every call.
type alwaysFullRunRetentionStore struct{ calls int }

func (a *alwaysFullRunRetentionStore) DeleteExpiredRuns(_ context.Context, _ time.Duration, limit int) (int, error) {
	a.calls++
	return limit, nil
}

// runRetentionRecorder keeps every audit row it is given, and whether its context was already done.
type runRetentionRecorder struct {
	mu       sync.Mutex
	evs      []types.AuditEvent
	doneCtxs int
}

func (r *runRetentionRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
	if ctx.Err() != nil {
		r.doneCtxs++
	}
	return nil
}

// cancellingRunRetentionStore deletes one batch and then ends the tick's context, as a lost leader term does.
type cancellingRunRetentionStore struct{ cancel context.CancelFunc }

func (c cancellingRunRetentionStore) DeleteExpiredRuns(context.Context, time.Duration, int) (int, error) {
	c.cancel()
	return 7, nil
}

func TestValidateRunHistoryRetentionDays(t *testing.T) {
	for _, n := range []int{0, 30, maxRetentionDays} {
		if err := validateRunHistoryRetentionDays(n); err != nil {
			t.Errorf("%d refused: %v", n, err)
		}
	}
	for _, n := range []int{-1, 1, 29, maxRetentionDays + 1} {
		if err := validateRunHistoryRetentionDays(n); err == nil {
			t.Errorf("%d accepted, want refused", n)
		}
	}
}

// A tick that deleted nothing writes no audit row; one that deleted runs writes one row with the total, also when
// a later batch failed, and returns that error.
func TestRunRetentionSweepTickAuditsOnlyDeletions(t *testing.T) {
	after := 30 * 24 * time.Hour
	ctx := context.Background()

	t.Run("nothing deleted", func(t *testing.T) {
		st, rec := &fakeRunRetentionStore{}, &runRetentionRecorder{}
		if err := runRetentionSweepTick(ctx, st, rec, after); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if st.calls != 1 || len(rec.evs) != 0 {
			t.Fatalf("calls = %d, audit rows = %d; want 1 and 0", st.calls, len(rec.evs))
		}
	})

	t.Run("three batches", func(t *testing.T) {
		st, rec := &fakeRunRetentionStore{script: []int{500, 500, 2}}, &runRetentionRecorder{}
		if err := runRetentionSweepTick(ctx, st, rec, after); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if st.calls != 3 {
			t.Fatalf("calls = %d, want 3", st.calls)
		}
		assertRunRetentionRow(t, rec, 1002, after)
	})

	t.Run("error after a batch", func(t *testing.T) {
		boom := errors.New("database down")
		st, rec := &fakeRunRetentionStore{script: []int{500, 0}, errAt: 2, err: boom}, &runRetentionRecorder{}
		if err := runRetentionSweepTick(ctx, st, rec, after); !errors.Is(err, boom) {
			t.Fatalf("tick = %v, want the store's error", err)
		}
		if st.calls != 2 {
			t.Fatalf("calls = %d, want 2", st.calls)
		}
		assertRunRetentionRow(t, rec, 500, after)
	})
}

func assertRunRetentionRow(t *testing.T, rec *runRetentionRecorder, deleted int, after time.Duration) {
	t.Helper()
	if len(rec.evs) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rec.evs))
	}
	ev := rec.evs[0]
	if ev.Action != "run.retention.sweep" || ev.ActorType != types.ActorSystem || ev.Actor != "wardyn/run-retention-sweeper" || ev.Outcome != "success" {
		t.Errorf("row = %s %s %s %s, want run.retention.sweep by the system sweeper, success", ev.Action, ev.ActorType, ev.Actor, ev.Outcome)
	}
	var data struct {
		Deleted      int   `json:"deleted"`
		RetentionSec int64 `json:"retention_sec"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("data: %v", err)
	}
	if data.Deleted != deleted || data.RetentionSec != int64(after.Seconds()) {
		t.Errorf("data = %+v, want deleted %d, retention_sec %d", data, deleted, int64(after.Seconds()))
	}
}

// One tick deletes at most runRetentionMaxBatches batches, however much is left.
func TestRunRetentionSweepTickStopsAtMaxBatches(t *testing.T) {
	st, rec := &alwaysFullRunRetentionStore{}, &runRetentionRecorder{}
	if err := runRetentionSweepTick(context.Background(), st, rec, 30*24*time.Hour); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if st.calls != 20 {
		t.Fatalf("calls = %d, want 20", st.calls)
	}
	assertRunRetentionRow(t, rec, 20*runRetentionBatch, 30*24*time.Hour)
}

// Unset, the window is 0: every run is kept.
func TestRunHistoryRetentionFlagDefaultsToKeepForever(t *testing.T) {
	ensureUnset(t, "WARDYN_RUN_HISTORY_RETENTION_DAYS")
	prevArgs, prevFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args = prevArgs; flag.CommandLine = prevFlags })
	resetFlags(t)
	os.Args = []string{"wardynd-test"}
	if got := *parseBootFlags().runHistoryRetention; got != 0 {
		t.Fatalf("default WARDYN_RUN_HISTORY_RETENTION_DAYS = %d, want 0 (keep forever)", got)
	}
}

// The audit row of a tick whose context ended after its deletes is still recorded, on a context that is not done.
func TestRunRetentionSweepTickAuditsAfterTheTermEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &runRetentionRecorder{}
	if err := runRetentionSweepTick(ctx, cancellingRunRetentionStore{cancel: cancel}, rec, 30*24*time.Hour); err != nil {
		t.Fatalf("tick: %v", err)
	}
	assertRunRetentionRow(t, rec, 7, 30*24*time.Hour)
	if rec.doneCtxs != 0 {
		t.Fatal("the audit row was recorded on the cancelled tick context")
	}
}
