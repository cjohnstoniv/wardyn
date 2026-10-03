// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CreateRunUnderCap_TwoInstancesAdmitExactlyTheCap races creators over two
// stores that share one database but not a connection pool, as two replicas do.
// Count-then-insert without the advisory lock admits more than the cap here.
func TestPG_CreateRunUnderCap_TwoInstancesAdmitExactlyTheCap(t *testing.T) {
	poolA := runsPGPoolIsolated(t)
	poolB, err := db.Connect(context.Background(), poolA.Config().ConnString())
	if err != nil {
		t.Fatalf("second instance connect: %v", err)
	}
	t.Cleanup(poolB.Close)
	instances := []store.PG{store.NewPG(poolA), store.NewPG(poolB)}

	const limit, creators = 3, 24
	var admitted, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < creators; i++ {
		wg.Add(1)
		go func(pg store.PG) {
			defer wg.Done()
			_, err := pg.CreateRunUnderCap(context.Background(), newRun(types.RunPending), limit)
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.Is(err, store.ErrRunCapReached):
				refused.Add(1)
			default:
				t.Errorf("create: %v", err)
			}
		}(instances[i%2])
	}
	wg.Wait()
	if admitted.Load() != limit || refused.Load() != creators-limit {
		t.Fatalf("admitted %d, refused %d; want exactly %d admitted and %d refused", admitted.Load(), refused.Load(), limit, creators-limit)
	}
}

// TestPG_CreateRunUnderCap_CountsRowsAndFreesOnTerminal pins that the cap counts
// non-terminal rows (a finished run frees a slot) and that 0 is unlimited.
func TestPG_CreateRunUnderCap_CountsRowsAndFreesOnTerminal(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	first, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1)
	if err != nil {
		t.Fatalf("first create under cap 1: %v", err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1); !errors.Is(err, store.ErrRunCapReached) {
		t.Fatalf("second create under cap 1 = %v, want ErrRunCapReached", err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 0); err != nil {
		t.Fatalf("cap 0 must be unlimited: %v", err)
	}
	if ok, err := pg.UpdateRunStateIf(ctx, first.ID, types.RunPending, types.RunFailed); err != nil || !ok {
		t.Fatalf("fail the first run: ok=%v err=%v", ok, err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 1 {
		t.Fatalf("CountNonTerminalRuns = %d, %v; want 1 (the failed run is terminal)", n, err)
	}
	// One PENDING row is still held (the cap-0 insert), so cap 1 refuses and cap 2 admits.
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1); !errors.Is(err, store.ErrRunCapReached) {
		t.Fatalf("create at cap 1 with one live row = %v, want ErrRunCapReached", err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 2); err != nil {
		t.Fatalf("create at cap 2 with one live row (the other is terminal): %v", err)
	}
}
