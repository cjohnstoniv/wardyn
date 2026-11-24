// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// holdingOrphanRunner is sandboxOrphanRunner that also implements the holding sweep: a row-less orphan the
// control plane's held answers true for is kept, any other orphan is torn down.
type holdingOrphanRunner struct {
	sandboxOrphanRunner
	kept  []uuid.UUID
	plain int // calls of the plain sweep
}

func (r *holdingOrphanRunner) SweepOrphanedSandboxes(ctx context.Context, minAge time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	r.plain++
	return r.sandboxOrphanRunner.SweepOrphanedSandboxes(ctx, minAge, isOrphan)
}

func (r *holdingOrphanRunner) SweepOrphanedSandboxesHolding(_ context.Context, _ time.Duration, isOrphan, held func(uuid.UUID) bool) (int, int, error) {
	var n, heldN int
	for _, id := range r.live {
		if !isOrphan(id) {
			continue
		}
		if held(id) {
			r.kept = append(r.kept, id)
			heldN++
			continue
		}
		r.tornDown = append(r.tornDown, id)
		n++
	}
	return n, heldN, nil
}

// orphanHoldFixture is a server over a store with one terminal ref-empty run and one live run; a third id, rowless,
// has no run row at all.
type orphanHoldFixture struct {
	srv      *Server
	rn       *holdingOrphanRunner
	rowless  uuid.UUID
	terminal uuid.UUID
	live     uuid.UUID
	now      time.Time
}

func newOrphanHoldFixture(t *testing.T, hold time.Duration) *orphanHoldFixture {
	t.Helper()
	h := newHarness(t)
	f := &orphanHoldFixture{rowless: uuid.New(), terminal: uuid.New(), live: uuid.New(), now: time.Now()}
	st := &sandboxOrphanStore{runs: []types.AgentRun{
		{ID: f.terminal, State: types.RunFailed, SandboxRef: ""},
		{ID: f.live, State: types.RunRunning, SandboxRef: "sandbox-live"},
	}}
	f.rn = &holdingOrphanRunner{sandboxOrphanRunner: sandboxOrphanRunner{fakeRunner: &fakeRunner{}, live: []uuid.UUID{f.rowless, f.terminal, f.live}}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = f.rn
	cfg.OrphanSweepHold = hold
	f.srv = New(cfg)
	f.srv.cfg.Now = func() time.Time { return f.now }
	return f
}

func (f *orphanHoldFixture) sweep(t *testing.T) {
	t.Helper()
	f.rn.kept, f.rn.tornDown = nil, nil
	if err := f.srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatalf("sweepOrphanedSandboxes: %v", err)
	}
}

func TestOrphanSweepHold_ARowlessSandboxIsHeldInsideTheHold(t *testing.T) {
	f := newOrphanHoldFixture(t, time.Hour)
	f.sweep(t)
	if !slices.Equal(f.rn.kept, []uuid.UUID{f.rowless}) {
		t.Fatalf("held %v, want only the sandbox with no run row", f.rn.kept)
	}
	if slices.Contains(f.rn.tornDown, f.rowless) {
		t.Error("a held sandbox was also torn down")
	}
	f.now = f.now.Add(30 * time.Minute)
	f.sweep(t)
	if !slices.Contains(f.rn.kept, f.rowless) {
		t.Errorf("half way through the hold the sandbox was released: held %v, torn down %v", f.rn.kept, f.rn.tornDown)
	}
}

func TestOrphanSweepHold_PastTheHoldItIsSwept(t *testing.T) {
	f := newOrphanHoldFixture(t, time.Hour)
	f.sweep(t)
	f.now = f.now.Add(time.Hour + time.Second)
	f.sweep(t)
	if slices.Contains(f.rn.kept, f.rowless) || !slices.Contains(f.rn.tornDown, f.rowless) {
		t.Fatalf("past the hold: held %v, torn down %v; want the sandbox torn down", f.rn.kept, f.rn.tornDown)
	}
}

func TestOrphanSweepHold_OffIsUnchanged(t *testing.T) {
	f := newOrphanHoldFixture(t, 0)
	f.sweep(t)
	if f.rn.plain != 1 || len(f.rn.kept) != 0 || !slices.Contains(f.rn.tornDown, f.rowless) {
		t.Fatalf("plain sweeps %d, held %v, torn down %v; want one plain sweep and the row-less sandbox torn down", f.rn.plain, f.rn.kept, f.rn.tornDown)
	}
}

func TestOrphanSweepHold_ATerminalRunsSandboxIsNeverHeld(t *testing.T) {
	f := newOrphanHoldFixture(t, time.Hour)
	f.sweep(t)
	if slices.Contains(f.rn.kept, f.terminal) || !slices.Contains(f.rn.tornDown, f.terminal) {
		t.Fatalf("held %v, torn down %v; want the terminal run's sandbox torn down", f.rn.kept, f.rn.tornDown)
	}
	if slices.Contains(f.rn.kept, f.live) || slices.Contains(f.rn.tornDown, f.live) {
		t.Error("a live run's sandbox was touched")
	}
}

func TestOrphanSweepHold_AServerWithoutAHolderSweepsAsToday(t *testing.T) {
	h := newHarness(t)
	rowless := uuid.New()
	cfg := baseTestConfig(h, &sandboxOrphanStore{})
	rn := &sandboxOrphanRunner{fakeRunner: &fakeRunner{}, live: []uuid.UUID{rowless}}
	cfg.Runner = rn
	cfg.OrphanSweepHold = time.Hour
	if err := New(cfg).sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatalf("sweepOrphanedSandboxes: %v", err)
	}
	if !slices.Contains(rn.tornDown, rowless) {
		t.Fatalf("torn down %v; a runner with no holding sweep must sweep as today", rn.tornDown)
	}
}
