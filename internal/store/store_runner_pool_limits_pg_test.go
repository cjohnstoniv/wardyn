// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func poolLimitsFixture(t *testing.T, maxRuns int) *types.RunnerPoolLimits {
	t.Helper()
	l := &types.RunnerPoolLimits{
		Background: &types.RunnerPoolBackgroundLimits{
			CPUMillis: types.RunnerPoolAmount{Default: 500, Cap: 2000},
			MemoryMiB: types.RunnerPoolAmount{Default: 512, Cap: 4096},
			Lifetime:  types.RunnerPoolDuration{DefaultSec: 3600, Unlimited: true},
		},
		Interactive: &types.RunnerPoolInteractiveLimits{
			CPUMillis: types.RunnerPoolAmount{Default: 1000, Cap: 4000},
			MemoryMiB: types.RunnerPoolAmount{Default: 1024, Cap: 8192},
			Lifetime:  types.RunnerPoolDuration{DefaultSec: 1800, MaxSec: 28800},
			Idle:      types.RunnerPoolDuration{DefaultSec: 600, MaxSec: 3600},
		},
		Barriers:          []types.ConfinementClass{types.CC1, types.CC2},
		MaxConcurrentRuns: &maxRuns,
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("the fixture must be valid: %v", err)
	}
	return l
}

// Limits come back field for field from the single read and from the catalogue every consumer reads;
// a pool created without them reads nil.
func TestPG_RunnerPoolLimitsRoundTrip(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	want := poolLimitsFixture(t, 3)
	limited, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Limited", HostingType: types.RunnerPoolRemoteProvided, Limits: want}, "test")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Plain", HostingType: types.RunnerPoolSelfHosted, Owner: "nobody"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(limited.Limits, want) || plain.Limits != nil {
		t.Errorf("create returned limits %+v / %+v", limited.Limits, plain.Limits)
	}
	got, err := pg.GetRunnerPool(ctx, limited.ID)
	if err != nil || !reflect.DeepEqual(got.Limits, want) {
		t.Errorf("GetRunnerPool limits = %+v, %v; want %+v", got.Limits, err, want)
	}
	if got, err := pg.GetRunnerPool(ctx, plain.ID); err != nil || got.Limits != nil {
		t.Errorf("a pool created without limits reads %+v, %v", got.Limits, err)
	}
	rows, err := pg.ListRunnerPoolCatalogue(ctx, "nobody", false)
	if err != nil || len(rows) != 2 {
		t.Fatalf("catalogue = %d rows, %v", len(rows), err)
	}
	for _, r := range rows {
		switch r.Pool.ID {
		case limited.ID:
			if !reflect.DeepEqual(r.Pool.Limits, want) {
				t.Errorf("catalogue limits = %+v, want %+v", r.Pool.Limits, want)
			}
		case plain.ID:
			if r.Pool.Limits != nil {
				t.Errorf("catalogue limits of a plain pool = %+v", r.Pool.Limits)
			}
		}
	}
}

// A stored pool read back from the store gives the contract the stored values, so the queue and the
// per-candidate checks read real storage.
func TestPG_RunnerPoolLimitsFeedTheContract(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	created, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Capped", HostingType: types.RunnerPoolRemoteProvided, Limits: poolLimitsFixture(t, 2)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	p, err := pg.GetRunnerPool(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	src, refusal := runnerpool.PoolSource(p, types.RunnerPoolRunInteractive)
	if refusal != nil || src.CPUMillis.Max == nil || *src.CPUMillis.Max != 4000 || src.IdleSec.Max == nil || *src.IdleSec.Max != 3600 {
		t.Errorf("PoolSource = %+v, %v; want the stored interactive cap 4000 and idle maximum 3600", src, refusal)
	}
	if runnerpool.AtCapacity(p, 2) == nil || runnerpool.AtCapacity(p, 1) != nil {
		t.Errorf("AtCapacity must wait at 2 and not at 1 for a stored cap of 2")
	}
	if r := runnerpool.CheckBarrier(p, types.CC3); r == nil {
		t.Errorf("a barrier outside the stored list must be refused")
	}
}
