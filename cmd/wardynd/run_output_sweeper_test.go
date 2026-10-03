// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type fakeOutputSweeper struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeOutputSweeper) SweepRunOutputs(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakeOutputSweeper) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func waitTick(t *testing.T, ms *sweephealth.MemStore, ok func(sweephealth.Tick) bool) sweephealth.Tick {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		ticks, err := ms.Ticks(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tk := ticks[sweephealth.RunOutput]; ok(tk) {
			return tk
		}
		if time.Now().After(deadline) {
			t.Fatal("the run_output sweep never recorded the tick the test waits for")
		}
	}
}

// A sweep tick writes sweep_ticks attempted_at and succeeded_at for run_output;
// a tick that errors moves the attempt and not the success.
func TestRunOutputSweeper_RecordsItsTick(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantSuccess bool
	}{
		{"success", nil, true},
		{"error", errors.New("postgres is down"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := sweephealth.NewMemStore()
			tr := sweephealth.New(ms, "r1", nil)
			srv := &fakeOutputSweeper{err: tc.err}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			startRunOutputSweeper(ctx, nil, srv, 5*time.Millisecond, tr) // nil leader: runs unconditionally
			tk := waitTick(t, ms, func(tk sweephealth.Tick) bool { return !tk.AttemptedAt.IsZero() })
			if tc.wantSuccess {
				tk = waitTick(t, ms, func(tk sweephealth.Tick) bool { return !tk.SucceededAt.IsZero() })
			}
			if srv.count() == 0 {
				t.Fatal("the sweep never ran")
			}
			if !tc.wantSuccess && !tk.SucceededAt.IsZero() {
				t.Fatalf("a tick that errored recorded a success: %+v", tk)
			}
		})
	}
}

// run_output is registered, hourly, only when persistence is on.
func TestRegisterSweepHealth_RunOutputFollowsPersistence(t *testing.T) {
	for _, on := range []bool{true, false} {
		clk := &healthClock{t: time.Now()}
		tr := registeredFor(t, sweepInstall{runOutputPersist: on}, clk)
		var got time.Duration
		for _, s := range tr.Registered() {
			if s.Name == sweephealth.RunOutput {
				got = s.Interval
			}
		}
		if want := map[bool]time.Duration{true: time.Hour, false: 0}[on]; got != want {
			t.Errorf("persist=%v: run_output registered at %v, want %v", on, got, want)
		}
	}
}

// orderRunner records the teardown call in the same list the output finaliser
// reports into, so the order of the two is the test's evidence.
type orderRunner struct {
	runner.Runner
	mu    *sync.Mutex
	order *[]string
}

func (r orderRunner) StopSandbox(context.Context, string) error {
	r.mu.Lock()
	*r.order = append(*r.order, "stop")
	r.mu.Unlock()
	return nil
}

// The idle stop reaches the output contract through the server's function, after
// the sandbox is stopped: the barrier then waits on a process that has exited.
func TestPG_IdleStopFinishesTheRunsOutputAfterStopSandbox(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	run := skewedRun(t, pg, 60)
	if err := pg.SetSandboxRef(ctx, run.ID, "sbx-idle-out"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var finished []uuid.UUID
	stopper := lifecycleStopper{
		pool:   pool,
		runner: orderRunner{mu: &mu, order: &order},
		finishOutput: func(_ context.Context, id uuid.UUID) {
			mu.Lock()
			order = append(order, "finish")
			finished = append(finished, id)
			mu.Unlock()
		},
	}
	out, err := stopper.StopRun(ctx, run.ID, time.Now().Add(time.Hour))
	if err != nil || !out.Applied {
		t.Fatalf("StopRun = %+v, %v; want applied", out, err)
	}
	if !slices.Equal(order, []string{"stop", "finish"}) || !slices.Equal(finished, []uuid.UUID{run.ID}) {
		t.Fatalf("order %v finished %v, want stop then finish for the run", order, finished)
	}
	if got, _ := pg.GetRun(ctx, run.ID); got.State != types.RunStopped {
		t.Fatalf("state %s, want STOPPED", got.State)
	}

	// A stop that loses the idle CAS finishes nothing.
	order, finished = nil, nil
	if out, err := stopper.StopRun(ctx, run.ID, time.Now().Add(time.Hour)); err != nil || out.Applied || len(order) != 0 {
		t.Fatalf("second StopRun = %+v, %v, order %v; want no-op", out, err, order)
	}
}
