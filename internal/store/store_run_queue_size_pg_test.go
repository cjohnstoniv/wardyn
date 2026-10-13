// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The promotion holds each runner to the size the run would start with THERE (the verdict's), under the
// runner's lock: a run that joined the queue at a size the small runner never takes is promoted onto it
// at the size it starts with there, that size is what it then reserves, and the runner takes no more
// runs of that size than its totals hold though it has runs at once to spare.
func TestPG_PromotionSizesTheRunForEachRunner(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 10)
	busy := capacityRunner(t, pg, 1)
	if _, err := pg.CreateRun(ctx, onRunner(p, busy, types.RunRunning)); err != nil {
		t.Fatal(err)
	}
	const small = "rp3-small"
	remoteRunner(t, pg, small, sizedLimits(3, 2))
	joined := oneRun()
	joined.DiskMiB *= 4 // more than the small runner takes a run
	now := time.Now().UTC()
	var queued []uuid.UUID
	for i := range 3 {
		waiting := onRunner(p, busy, types.RunQueued)
		waiting.Reserved = joined
		at := now.Add(time.Duration(i) * time.Millisecond)
		r, err := pg.CreateQueuedRun(ctx, waiting, store.RunQueueEntry{QueuedAt: at, DeadlineAt: at.Add(time.Hour)}, 0)
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, r.ID)
	}
	// The verdict sizes the run for the small runner; the busy one is held to the size the run joined with.
	sized := func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
		return store.QueueVerdict{Admit: true, Runners: []uuid.UUID{busy.ID}, Remotes: []string{small}, Needs: map[string]types.RunnerAmounts{small: oneRun()}}, nil
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), sized)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("promoted %d runs, want the two the small runner's totals hold at the size they start with there", len(got))
	}
	for _, g := range got {
		if g.Run.RunnerID != nil || g.Run.RunnerPoolExecutor != small {
			t.Errorf("run %s promoted onto runner %v executor %q, want %s alone", g.Run.ID, g.Run.RunnerID, g.Run.RunnerPoolExecutor, small)
		}
	}
	if r := runState(t, pg, queued[2]); r.State != types.RunQueued {
		t.Errorf("the third run is %s, want it still waiting: the small runner's totals are spent", r.State)
	}
	used, err := pg.RunnerUsage(ctx, nil, []string{small}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	one := oneRun()
	if want := (types.RunnerTally{Runs: 2, CPUMillis: 2 * one.CPUMillis, MemoryMiB: 2 * one.MemoryMiB, DiskMiB: 2 * one.DiskMiB}); used[small] != want {
		t.Errorf("in use on the small runner = %+v, want %+v: each run reserves the size it was promoted at", used[small], want)
	}
}

// The start holds its runner to the size the run STARTS with, either kind: a start naming a size past the
// runner's per-run limit is refused though the runner has a place free, and the same run starts at a size
// the runner takes.
func TestPG_StartHoldsTheRunnerToTheStartSize(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 10)
	own := newRunner("alice")
	own.Limits = sizedLimits(2, 2)
	own, err := pg.CreateRunner(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	const remote = "rp3-start"
	remoteRunner(t, pg, remote, sizedLimits(2, 2))
	onOwn, err := enqueueOnRunner(t, pg, p, own)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	onRemote, err := pg.CreateQueuedRun(ctx, poolRun(p, types.RunQueued, "alice"), store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	each := func(_ context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		if e.RunID == onOwn.ID {
			return store.QueueVerdict{Admit: true, Runners: []uuid.UUID{own.ID}}, nil
		}
		return store.QueueVerdict{Admit: true, Remotes: []string{remote}}, nil
	}
	if got, _, err := pg.PromoteRunQueue(ctx, pass(p), each); err != nil || len(got) != 2 {
		t.Fatalf("promoted %d (%v), want both runs", len(got), err)
	}
	tooBig := oneRun()
	tooBig.CPUMillis++
	for name, start := range map[string]func(types.RunnerAmounts) (types.AgentRun, bool, error){
		"self-hosted": func(size types.RunnerAmounts) (types.AgentRun, bool, error) {
			return pg.StartQueuedRun(ctx, onOwn.ID, store.QueueStart{RunnerID: &own.ID, Reserved: size, Placement: types.PlacementLocal, ConfinementClass: types.CC2})
		},
		"remote-hosted": func(size types.RunnerAmounts) (types.AgentRun, bool, error) {
			return pg.StartQueuedRun(ctx, onRemote.ID, store.QueueStart{RunnerPoolExecutor: remote, Reserved: size, Placement: types.PlacementRemote, ConfinementClass: types.CC2})
		},
	} {
		if _, _, err := start(tooBig); !errors.Is(err, store.ErrRunnerAtCapacity) {
			t.Errorf("%s: a start past the runner's per-run limit = %v, want ErrRunnerAtCapacity", name, err)
		}
		if _, ok, err := start(oneRun()); err != nil || !ok {
			t.Errorf("%s: a start at a size the runner takes = %v %v, want it started", name, ok, err)
		}
	}
}
