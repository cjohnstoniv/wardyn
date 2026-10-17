// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

type countingPools struct {
	store.RunnerPoolStore
	reads int
}

func (c *countingPools) ListRunnerPoolCatalogue(context.Context, string, bool) ([]store.RunnerPoolCatalogueRow, error) {
	c.reads++
	return nil, nil
}

// TestQueuePass_ReadsAnOwnersPoolsOncePerPass: the waiting runs of one owner under one authority share
// one read of the owner's pools within a pass; another authority, or a judgement outside any pass,
// reads again.
func TestQueuePass_ReadsAnOwnersPoolsOncePerPass(t *testing.T) {
	srv, _, _ := newRunnerRegistrationServer(t)
	ps := &countingPools{}
	read := func(ctx context.Context) {
		t.Helper()
		if _, refusal := srv.queuePermittedPools(ctx, ps); refusal != nil {
			t.Fatalf("queuePermittedPools: %+v", refusal)
		}
	}
	alice := withOIDCHuman(context.Background(), "alice")

	pass := withQueuePass(alice)
	read(withQueueOwner(pass, "alice", []byte(`{"sub":"alice"}`)))
	read(withQueueOwner(pass, "alice", []byte(`{"sub":"alice"}`)))
	if ps.reads != 1 {
		t.Fatalf("two runs under one authority read the pools %d times, want 1", ps.reads)
	}
	read(withQueueOwner(pass, "alice", []byte(`{"sub":"alice","user_type":"contractor"}`)))
	if ps.reads != 2 {
		t.Fatalf("a run under another authority read the pools %d times in all, want 2", ps.reads)
	}
	read(withQueueOwner(withQueuePass(alice), "alice", []byte(`{"sub":"alice"}`)))
	if ps.reads != 3 {
		t.Fatalf("a new pass read the pools %d times in all, want 3 (no fact outlives its pass)", ps.reads)
	}
	read(alice)
	read(alice)
	if ps.reads != 5 {
		t.Fatalf("two judgements outside a pass read the pools %d times in all, want 5", ps.reads)
	}
}
