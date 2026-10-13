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
